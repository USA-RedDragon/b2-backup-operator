package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

const redacted = "***"

func newEnvCommand(f *Factory) *cobra.Command {
	var showPassword bool

	cmd := &cobra.Command{
		Use:     "env [NAME]",
		Short:   "Print the restic environment for a repository",
		GroupID: GroupEscape,
		Long: `Print the restic credentials as shell export statements.

RESTIC_PASSWORD and AWS_SECRET_ACCESS_KEY are redacted unless --show-password
is passed. The rest, the s3: repository URL, the application key ID and the
region, are not secret and are printed as they are.

With --show-password the output is suitable for:

    eval "$(b2bk env repo/store --show-password)"

With no NAME, it uses the namespace's only ScheduledBackup, or its only
Repository when it has no ScheduledBackup.`,
		Args: argCount(0, 1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			arg, err := f.nameArg(cmd, args, anyKind)
			if err != nil {
				return err
			}
			return runEnv(cmd.Context(), c, f.Streams.Out, f.Streams.ErrOut, ns, arg, showPassword)
		},
	}

	cmd.Flags().BoolVar(&showPassword, "show-password", false,
		"Print the real password instead of redacting it")
	return cmd
}

func runEnv(
	ctx context.Context,
	c client.Client,
	out, errOut io.Writer,
	namespace, arg string,
	showPassword bool,
) error {
	target, err := Resolve(ctx, c, namespace, arg)
	if err != nil {
		return err
	}

	repo := target.Repository
	if target.Kind == TargetScheduledBackup {
		if repo, err = RepositoryFor(ctx, c, target.ScheduledBackup); err != nil {
			return err
		}
	}

	secret, err := credentials(ctx, c, repo)
	if err != nil {
		return err
	}

	hidden := false
	p := newPrinter(out)
	for _, key := range controller.SecretKeys {
		value := string(secret.Data[key])
		if !showPassword && (key == controller.KeyResticPassword || key == controller.KeySecretAccessKey) {
			value = redacted
			hidden = true
		}
		p.printf("export %s=%s\n", key, shellQuote(value))
	}
	if err := p.Err(); err != nil {
		return err
	}

	if hidden {
		hint := newPrinter(errOut)
		hint.println("# RESTIC_PASSWORD and AWS_SECRET_ACCESS_KEY redacted; pass --show-password to reveal them")
		return hint.Err()
	}
	return nil
}

func credentials(ctx context.Context, c client.Client, repo *b2backupv1.Repository) (*corev1.Secret, error) {
	name := repo.SecretName()
	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: repo.Namespace, Name: name}, &secret)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: credentials Secret %q for repository %q; run: b2bk doctor repo/%s",
			ErrTargetNotFound, name, repo.Name, repo.Name)
	}
	if err != nil {
		return nil, err
	}
	return &secret, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
