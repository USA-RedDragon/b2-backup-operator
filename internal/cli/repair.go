package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
	"github.com/USA-RedDragon/b2-backup-operator/internal/secrets"
)

// ErrNotInitialized means restic init has not run against the repository yet.
var ErrNotInitialized = errors.New("repository is not initialized")

func newReinitCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "reinit <repository>",
		Short:   "Re-run repository initialization",
		GroupID: GroupLifecycle,
		Long: `Make the operator run restic init again.

A failed init Job is retried indefinitely on a five minute delay: the operator
deletes the failed Job and builds a fresh one on the next pass. If it failed for
a reason since fixed -- a bad credential, a network policy -- this skips the
wait by setting the ` + b2backupv1.AnnotationReinitAt + ` annotation. The operator
then deletes its init Job, clears status.initialized and starts over now.

b2bk never writes a Repository's status: what status records decides what the
operator may delete, so only the operator writes it.

This does not touch the repository's contents. The init Job probes with
restic cat config first, and restic init refuses a repository that already
holds a config.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			target, err := Resolve(cmd.Context(), c, ns, args[0])
			if err != nil {
				return err
			}
			if target.Kind != TargetRepository {
				return fmt.Errorf("reinit needs a Repository, not a %s", target.Kind)
			}
			return runReinit(cmd.Context(), c, f.Streams.Out, target.Repository, time.Now())
		},
	}
	return cmd
}

func runReinit(
	ctx context.Context, c client.Client, out io.Writer, repo *b2backupv1.Repository, now time.Time,
) error {
	patch := client.MergeFrom(repo.DeepCopy())
	if repo.Annotations == nil {
		repo.Annotations = map[string]string{}
	}
	repo.Annotations[b2backupv1.AnnotationReinitAt] = now.UTC().Format(time.RFC3339)
	if err := c.Patch(ctx, repo, patch); err != nil {
		return fmt.Errorf("annotating repository/%s: %w", repo.Name, err)
	}

	p := newPrinter(out)
	p.printf("asked the operator to initialize repository/%s again\n", repo.Name)
	p.printf("  watch it with: b2bk doctor repo/%s\n", repo.Name)
	return p.Err()
}

func newRotatePasswordCommand(f *Factory) *cobra.Command {
	var archived bool

	cmd := &cobra.Command{
		Use:     "rotate-password <repository>",
		Short:   "Add a new restic encryption key",
		GroupID: GroupLifecycle,
		Long: `Rotate the restic encryption password.

This adds a new key to the repository and points the credentials Secret at it.
The old password is deliberately left working, so a snapshot taken before the
rotation is still readable and the change can be undone by restoring the old
Secret. Remove the old key yourself once you are sure:

    b2bk exec REPOSITORY -- restic key list
    b2bk exec REPOSITORY -- restic key remove <id>

The password is the only thing that can decrypt this repository. If the Secret
is lost and the password was not archived off-cluster, every snapshot is
unreadable forever -- which is why this refuses to run without the flag saying
you have somewhere to put it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !archived {
				return errors.New(
					"refusing to rotate without --i-have-somewhere-to-archive-the-new-password: " +
						"the password is the only thing that can decrypt this repository")
			}

			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			sb, repo, err := resolveRunTarget(cmd.Context(), c, ns, args[0])
			if err != nil {
				return err
			}
			return runRotatePassword(cmd.Context(), f, c, sb, repo)
		},
	}

	cmd.Flags().BoolVar(&archived, "i-have-somewhere-to-archive-the-new-password", false,
		"Confirm the new password will be stored off-cluster")
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}

func newKeySecretName(repo *b2backupv1.Repository) string {
	return repo.Name + "-b2bk-newkey-" + rand.String(5)
}

const newKeyMountPath = "/newkey"

func runRotatePassword(
	ctx context.Context,
	f *Factory,
	c client.Client,
	sb *b2backupv1.ScheduledBackup,
	repo *b2backupv1.Repository,
) error {
	if !repo.Status.Initialized {
		return fmt.Errorf("%w: repository/%s has nothing to rotate yet", ErrNotInitialized, repo.Name)
	}
	if err := requireOwnRepository(repo); err != nil {
		return err
	}

	current, err := credentials(ctx, c, repo)
	if err != nil {
		return err
	}

	password, err := secrets.GeneratePassword()
	if err != nil {
		return err
	}

	temp := &corev1.Secret{
		Namespace: repo.Namespace,
		Name:      newKeySecretName(repo),
		Labels:    map[string]string{labelManagedBy: runner.ManagedByValue},
		Data:      map[string][]byte{"password": []byte(password)},
	}
	if err := c.Create(ctx, temp); err != nil {
		return fmt.Errorf("creating the temporary key Secret: %w", err)
	}

	defer func() {
		if err := deleteTempKeySecret(c, temp); err != nil {
			p := newPrinter(f.Streams.ErrOut)
			p.printf("! could not delete the temporary key secret/%s: %v\n", temp.Name, err)
			p.printf("! it holds the new password in plaintext; delete it with: kubectl -n %s delete secret %s\n",
				temp.Namespace, temp.Name)
		}
	}()

	run, err := f.Runner()
	if err != nil {
		return err
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("adding a new key to repository/%s\n", repo.Name)
	if err := p.Err(); err != nil {
		return err
	}

	err = run.Run(ctx, sb, runner.Options{
		Purpose: "rotate",
		Command: resticCommand("key", "add",
			"--new-password-file="+newKeyMountPath+"/password"),
		ExtraVolumes: []corev1.Volume{{
			Name:   "newkey",
			Secret: &corev1.SecretVolumeSource{SecretName: temp.Name},
		}},
		ExtraMounts: []corev1.VolumeMount{
			{Name: "newkey", MountPath: newKeyMountPath, ReadOnly: true},
		},
	}, f.Streams.Out, defaultResticTimeout)
	if err != nil {
		return fmt.Errorf("adding the new key: %w", err)
	}

	if err := pointSecretAtPassword(ctx, c, repo, current, password); err != nil {
		return err
	}

	done := newPrinter(f.Streams.Out)
	done.printf("rotated the encryption key for repository/%s\n", repo.Name)
	done.printf("  archive it now:   b2bk env repo/%s --show-password\n", repo.Name)
	for _, line := range staleSeedNotice(repo, current.Name) {
		done.printf("! %s\n", line)
	}
	done.printf("  the old key still works; remove it with:\n")
	done.printf("    b2bk exec %s -- restic key list\n", sb.Name)
	return done.Err()
}

// pointSecretAtPassword writes the new password into the credentials Secret,
// with the operator's protect-password finalizer in the same write: until the
// seed holds it, the Secret is the only copy, and the operator only adds the
// finalizer on its next pass.
func pointSecretAtPassword(
	ctx context.Context, c client.Client, repo *b2backupv1.Repository, secret *corev1.Secret, password string,
) error {
	patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if secret.Data == nil {
		secret.Data = make(map[string][]byte, 1)
	}
	secret.Data[controller.KeyResticPassword] = []byte(password)
	controllerutil.AddFinalizer(secret, controller.SecretFinalizer)
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[b2backupv1.AnnotationProtectedBy] = string(repo.UID)
	if err := c.Patch(ctx, secret, patch); err != nil {
		return fmt.Errorf("updating %s in secret/%s: %w", controller.KeyResticPassword, secret.Name, err)
	}
	return nil
}

// staleSeedNotice says where the old password still lives after a rotation.
func staleSeedNotice(repo *b2backupv1.Repository, secret string) []string {
	ref := repo.Spec.PasswordSecretRef
	if ref == nil {
		return []string{"any copy of the old password kept in Git or SOPS is now stale; replace it with the new one"}
	}
	return []string{
		fmt.Sprintf("spec.passwordSecretRef names secret/%s, and the %s it holds is now the old password.",
			ref.Name, ref.Key),
		"Update it, and its copy in Git or SOPS, with the new one. Until then the operator keeps",
		fmt.Sprintf("finalizer %s on secret/%s, since that is the only copy of the new password.",
			controller.SecretFinalizer, secret),
	}
}

func deleteTempKeySecret(c client.Client, temp *corev1.Secret) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
	defer cancel()

	err := c.Delete(ctx, &corev1.Secret{
		Namespace: temp.Namespace, Name: temp.Name,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
