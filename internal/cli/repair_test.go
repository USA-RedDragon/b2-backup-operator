package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

func TestNewKeySecretNameIsUniquePerCall(t *testing.T) {
	repo := &b2backupv1.Repository{Name: "myapp", Namespace: "acme-prod"}
	prefix := repo.Name + "-b2bk-newkey-"

	const draws = 200
	seen := make(map[string]struct{}, draws)
	for range draws {
		name := newKeySecretName(repo)

		if !strings.HasPrefix(name, prefix) {
			t.Fatalf("newKeySecretName() = %q, want the prefix %q", name, prefix)
		}
		if suffix := strings.TrimPrefix(name, prefix); len(suffix) != 5 {
			t.Fatalf("newKeySecretName() = %q, want a five character suffix, got %q", name, suffix)
		}
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			t.Fatalf("newKeySecretName() = %q is not a valid Secret name: %v", name, errs)
		}
		seen[name] = struct{}{}
	}
	if len(seen) < draws-10 {
		t.Errorf("newKeySecretName() returned %d distinct names in %d calls; the suffix is not "+
			"being redrawn, and a stale Secret would supply the password restic key add reads",
			len(seen), draws)
	}
}

const archiveFlag = "--i-have-somewhere-to-archive-the-new-password"

func initializedRepo() *b2backupv1.Repository {
	repo := newRepo(testNS)
	repo.Status.Initialized = true
	return repo
}

func tempKeySecrets(t *testing.T, c client.Client) []corev1.Secret {
	t.Helper()
	var list corev1.SecretList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	var left []corev1.Secret
	for _, s := range list.Items {
		if strings.Contains(s.Name, "-b2bk-newkey-") {
			left = append(left, s)
		}
	}
	return left
}

func TestRotatePasswordRefusesWithoutTheArchiveFlag(t *testing.T) {
	cmd := newRotatePasswordCommand(offlineFactory(t, testStreams()))
	cmd.SetArgs([]string{testRepoName})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("rotate-password ran without the flag saying the password will be archived")
	}
	if !strings.Contains(err.Error(), archiveFlag) {
		t.Errorf("error = %v, want it to name %s", err, archiveFlag)
	}
	if !strings.Contains(err.Error(), "only thing that can decrypt") {
		t.Errorf("error = %v, want it to say what is at stake", err)
	}
}

func TestRotatePasswordGuardRunsBeforeAnythingElse(t *testing.T) {
	cmd := newRotatePasswordCommand(offlineFactory(t, testStreams()))
	cmd.SetArgs([]string{testRepoName, archiveFlag})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected the offline client to fail")
	}
	if strings.Contains(err.Error(), archiveFlag) {
		t.Errorf("error = %v, want the flag to have been accepted", err)
	}
}

func TestRotatePasswordRefusesAnUninitializedRepository(t *testing.T) {
	repo := newRepo(testNS)
	sb := newBackup(testBackupName, testRepoName)
	c := newClient(t, repo, sb, credentialsSecret(repo.SecretName()))

	err := runRotatePassword(context.Background(), offlineFactory(t, testStreams()), c, sb, repo)
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("error = %v, want ErrNotInitialized", err)
	}
	if left := tempKeySecrets(t, c); len(left) > 0 {
		t.Errorf("a key Secret was created for a repository with nothing to rotate: %v", left)
	}
}

func TestRotatePasswordNeedsTheCredentialsSecret(t *testing.T) {
	repo := initializedRepo()
	sb := newBackup(testBackupName, testRepoName)
	c := newClient(t, repo, sb)

	err := runRotatePassword(context.Background(), offlineFactory(t, testStreams()), c, sb, repo)
	if err == nil {
		t.Fatal("expected an error when there is no Secret to point at the new key")
	}
	if left := tempKeySecrets(t, c); len(left) > 0 {
		t.Errorf("a key Secret was created before the target Secret was known to exist: %v", left)
	}
}

func TestRotatePasswordKeepsTheOldKeyWhenResticKeyAddFails(t *testing.T) {
	repo := initializedRepo()
	sb := newBackup(testBackupName, testRepoName)
	secret := credentialsSecret(repo.SecretName())
	before := string(secret.Data[controller.KeyResticPassword])

	var created *corev1.Secret
	c := clientBuilder(t, repo, sb, secret).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(
			ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption,
		) error {
			if s, ok := obj.(*corev1.Secret); ok && strings.Contains(s.Name, "-b2bk-newkey-") {
				created = s.DeepCopy()
			}
			return cl.Create(ctx, obj, opts...)
		},
	}).Build()

	err := runRotatePassword(context.Background(), offlineFactory(t, testStreams()), c, sb, repo)
	if err == nil {
		t.Fatal("expected restic key add against an unreachable cluster to fail")
	}
	if !strings.Contains(err.Error(), "adding the new key") {
		t.Errorf("error = %v, want it to say the key add failed", err)
	}

	if created == nil {
		t.Fatal("no temporary key Secret was created")
	}
	if created.Labels["app.kubernetes.io/managed-by"] != runner.ManagedByValue {
		t.Errorf("temporary key Secret labels = %v, want the managed-by label so cleanup finds it",
			created.Labels)
	}
	if len(created.Data["password"]) == 0 {
		t.Error("the temporary key Secret carries no password for restic to read")
	}

	var after corev1.Secret
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: repo.SecretName()}, &after); err != nil {
		t.Fatal(err)
	}
	got := string(after.Data[controller.KeyResticPassword])
	if got != before {
		t.Errorf("RESTIC_PASSWORD = %q, want the old %q; pointing the Secret at a key "+
			"restic never accepted makes every snapshot unreadable", got, before)
	}
	if got == string(created.Data["password"]) {
		t.Error("the Secret was pointed at the new key even though restic key add failed")
	}

	if left := tempKeySecrets(t, c); len(left) > 0 {
		t.Errorf("the temporary Secret holding the new password in plaintext was left behind: %v", left)
	}
}

func TestDeleteTempKeySecretToleratesAnAlreadyDeletedSecret(t *testing.T) {
	c := newClient(t)
	temp := &corev1.Secret{Namespace: testNS, Name: "store-b2bk-newkey-abcde"}
	if err := deleteTempKeySecret(c, temp); err != nil {
		t.Errorf("deleteTempKeySecret() = %v, want nil for a Secret already gone", err)
	}
}

func TestReinitDeletesOnlyTheJobsThisRepositoryOwns(t *testing.T) {
	repo := initializedRepo()
	mine := initJobFor(repo)
	other := newRepo(testNS)
	other.Name, other.UID = "otherrepo", types.UID("uid-repo-otherrepo")
	theirs := initJobFor(other)
	unowned := &batchv1.Job{Namespace: testNS, Name: "nightly-cleanup"}

	c := newClient(t, repo, mine, theirs, unowned)

	var out bytes.Buffer
	if err := runReinit(context.Background(), c, &out, repo); err != nil {
		t.Fatalf("runReinit: %v", err)
	}

	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(mine), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("the repository's own init Job survived reinit: %v", err)
	}
	for _, keep := range []*batchv1.Job{theirs, unowned} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(keep), &batchv1.Job{}); err != nil {
			t.Errorf("reinit deleted job/%s, which it does not own: %v", keep.Name, err)
		}
	}

	var after b2backupv1.Repository
	if err := c.Get(ctx, client.ObjectKeyFromObject(repo), &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Initialized {
		t.Error("status.initialized was left set, so the operator will not run restic init again")
	}

	for _, want := range []string{"deleted job/" + mine.Name, "b2bk doctor"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to mention %q", out.String(), want)
		}
	}
}

func TestReinitWorksWhenThereIsNoJobLeft(t *testing.T) {
	repo := initializedRepo()
	c := newClient(t, repo)

	var out bytes.Buffer
	if err := runReinit(context.Background(), c, &out, repo); err != nil {
		t.Fatalf("runReinit: %v", err)
	}

	var after b2backupv1.Repository
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Initialized {
		t.Error("status.initialized was left set with no Job to delete")
	}
}

func factoryWithClient(t *testing.T, c client.Client) *Factory {
	t.Helper()
	f := offlineFactory(t, testStreams())
	f.client = c
	f.once.client.Do(func() {})
	return f
}

func TestReinitNeedsARepository(t *testing.T) {
	repo := initializedRepo()
	sb := newBackup(testBackupName, testRepoName)
	c := newClient(t, repo, sb, initJobFor(repo))
	f := factoryWithClient(t, c)

	cmd := newReinitCommand(f)
	cmd.SetArgs([]string{subjectBackup})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("reinit accepted a ScheduledBackup; it clears Repository state")
	}
	if !strings.Contains(err.Error(), "reinit needs a Repository") {
		t.Errorf("error = %v, want it to say reinit needs a Repository", err)
	}

	var after b2backupv1.Repository
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), &after); err != nil {
		t.Fatal(err)
	}
	if !after.Status.Initialized {
		t.Error("the wrong kind of argument still cleared status.initialized")
	}

	cmd = newReinitCommand(f)
	cmd.SetArgs([]string{subjectRepo})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("reinit on a Repository: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(repo), &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Initialized {
		t.Error("reinit left status.initialized set")
	}
}
