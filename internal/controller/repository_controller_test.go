package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/borgbase"
)

const (
	testNS     = "myapp-prod"
	resticName = "restic"
	seedSecret = "restic-envs"
	tokenNS    = "b2backup-system"
	tokenName  = "borgbase-api"

	adoptedRepoID = "a1b2c3d4"
	adoptedName   = "adopted"
	legacyName    = "legacy-backups"
	otherRepoID   = "zzzzzzzz"

	credsSecretName = "restic-b2"

	wantInitPoll = 15 * time.Second
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := b2backupv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newHarness(t *testing.T, api *fakeAPI, objs ...client.Object) (*RepositoryReconciler, client.Client) {
	t.Helper()
	return newHarnessWithInterceptor(t, api, interceptor.Funcs{}, objs...)
}

func newHarnessWithInterceptor(
	t *testing.T, api *fakeAPI, funcs interceptor.Funcs, objs ...client.Object,
) (*RepositoryReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)

	all := append([]client.Object{&corev1.Secret{
		Name: tokenName, Namespace: tokenNS,
		Data: map[string][]byte{"token": []byte("test-token")},
	}}, objs...)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(all...).
		WithStatusSubresource(&b2backupv1.Repository{}).
		WithInterceptorFuncs(funcs).
		Build()

	return &RepositoryReconciler{
		Client:             c,
		Scheme:             scheme,
		Recorder:           events.NewFakeRecorder(100),
		NewAPI:             func(string) borgbase.API { return api },
		DefaultTokenSecret: types.NamespacedName{Namespace: tokenNS, Name: tokenName},
		DefaultTokenKey:    "token",
		BackupImage:        "ghcr.io/usa-reddragon/b2-backup-operator-restic:test",
	}, c
}

// failRepositoryList aborts a reconcile inside repositoryConflict, which lands
// between api.Add and the end-of-pass status patch.
func failRepositoryList(fail *bool) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(
			ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption,
		) error {
			if _, ok := list.(*b2backupv1.RepositoryList); ok && *fail {
				return errors.New("simulated apiserver failure")
			}
			return cl.List(ctx, list, opts...)
		},
	}
}

func repositoryFixture(mutate func(*b2backupv1.Repository)) *b2backupv1.Repository {
	repo := &b2backupv1.Repository{
		Name: resticName, Namespace: testNS,
		Spec: b2backupv1.RepositorySpec{
			Region:         "us",
			DeletionPolicy: b2backupv1.DeletionPolicyRetain,
		},
	}
	if mutate != nil {
		mutate(repo)
	}
	return repo
}

func reconcileN(t *testing.T, r *RepositoryReconciler, n int) {
	t.Helper()
	req := ctrl.Request{Namespace: testNS, Name: resticName}
	for i := range n {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &s); err != nil {
		t.Fatalf("getting secret %s: %v", name, err)
	}
	return &s
}

func TestPasswordIsGeneratedOnceAndNeverRotated(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))

	reconcileN(t, r, 1)
	first := string(getSecret(t, c, "restic-b2").Data[KeyResticPassword])
	if first == "" {
		t.Fatal("no password was written")
	}

	reconcileN(t, r, 5)
	after := string(getSecret(t, c, "restic-b2").Data[KeyResticPassword])
	if after != first {
		t.Errorf("password changed across reconciles:\n first = %q\n after = %q", first, after)
	}
}

func TestAdoptionNeverCreates(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic,
		Htpasswd: "secret-token", CurrentUsage: 3.2,
	})
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.ExistingRepositoryID = adoptedRepoID
		r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{
			Name: seedSecret,
			Key:  KeyResticPassword,
		}
	})
	seed := &corev1.Secret{
		Name: seedSecret, Namespace: testNS,
		Data: map[string][]byte{KeyResticPassword: []byte("existing-password-from-sops")},
	}

	r, c := newHarness(t, api, repo, seed)
	reconcileN(t, r, 3)

	if api.called("Add") {
		t.Error("adoption called repoAdd; it must only ever look the repository up")
	}

	secret := getSecret(t, c, "restic-b2")
	if got := string(secret.Data[KeyResticPassword]); got != "existing-password-from-sops" {
		t.Errorf("adopted password = %q, want the seeded one", got)
	}
	want := "rest:https://" + adoptedRepoID + ":secret-token@" + adoptedRepoID + ".repo.borgbase.com"
	if got := string(secret.Data[KeyResticRepository]); got != want {
		t.Errorf("RESTIC_REPOSITORY = %q, want %q", got, want)
	}

	var afterSeed corev1.Secret
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: seedSecret}, &afterSeed); err != nil {
		t.Fatal(err)
	}
	if string(afterSeed.Data[KeyResticPassword]) != "existing-password-from-sops" {
		t.Error("the operator modified the seed secret")
	}
}

func TestRefusesToInventPasswordForNonEmptyRepository(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: testRepoID, Name: testNS, Format: borgbase.FormatRestic,
		Htpasswd: "t", CurrentUsage: 12.5,
	})
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Status.RepositoryID = testRepoID
	})

	r, _ := newHarness(t, api, repo)
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName})
	if err == nil {
		t.Fatal("expected an error for a non-empty repository with no password")
	}
}

func TestRejectsNonResticRepository(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: "borgrepo", Name: "myapp-prod", Format: "borg", Htpasswd: "t",
	})
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.ExistingRepositoryID = "borgrepo"
		r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{
			Name: seedSecret,
			Key:  KeyResticPassword,
		}
	})
	seed := &corev1.Secret{
		Name: seedSecret, Namespace: testNS,
		Data: map[string][]byte{KeyResticPassword: []byte("pw")},
	}

	r, _ := newHarness(t, api, repo, seed)
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName})
	if err == nil {
		t.Fatal("expected an error for a non-restic repository")
	}
}

func TestCreatesRepositoryAndInitJob(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	if !api.called("Add") {
		t.Error("expected a repository to be created")
	}

	var job batchv1.Job
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: "restic-init"}, &job); err != nil {
		t.Fatalf("expected an init job: %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]

	want := "restic cat config >/dev/null 2>&1 || restic init"
	if got := container.Command[len(container.Command)-1]; got != want {
		t.Errorf("init command = %q, want %q", got, want)
	}
	if container.EnvFrom[0].SecretRef.Name != "restic-b2" {
		t.Errorf("init job reads the wrong secret: %s", container.EnvFrom[0].SecretRef.Name)
	}

	if automount := job.Spec.Template.Spec.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("init job should not mount a service account token")
	}

	if sc := container.SecurityContext; sc == nil || sc.Capabilities == nil ||
		len(sc.Capabilities.Drop) == 0 {
		t.Error("init container should drop all capabilities")
	}

	if container.Resources.Requests.Cpu().IsZero() || container.Resources.Requests.Memory().IsZero() {
		t.Error("init job should set resource requests")
	}
}

func TestSucceededInitJobIsRemovedOnTheNextPass(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	key := types.NamespacedName{Namespace: testNS, Name: "restic-init"}
	var job batchv1.Job
	if err := c.Get(context.Background(), key, &job); err != nil {
		t.Fatalf("expected an init job: %v", err)
	}

	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}

	reconcileN(t, r, 1)
	var repo b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &repo); err != nil {
		t.Fatal(err)
	}
	if !repo.Status.Initialized {
		t.Fatal("repository should be marked initialized")
	}
	if err := c.Get(context.Background(), key, &job); err != nil {
		t.Errorf("init job should survive the pass that records success: %v", err)
	}

	reconcileN(t, r, 1)
	if err := c.Get(context.Background(), key, &job); !apierrors.IsNotFound(err) {
		t.Errorf("init job should have been deleted, got %v", err)
	}

	reconcileN(t, r, 3)
	if err := c.Get(context.Background(), key, &job); !apierrors.IsNotFound(err) {
		t.Error("a second init job was created after initialization completed")
	}
}

func TestDeletionPolicyRetainKeepsRepositoryAndSecret(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	var repo b2backupv1.Repository
	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	if err := c.Get(context.Background(), key, &repo); err != nil {
		t.Fatal(err)
	}
	id := repo.Status.RepositoryID
	if id == "" {
		t.Fatal("no repository id recorded")
	}
	if err := c.Delete(context.Background(), &repo); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)

	if api.called("Delete") {
		t.Error("Retain called repoDelete; backups must survive deleting the resource")
	}
	if _, ok := api.repos[id]; !ok {
		t.Error("the BorgBase repository was removed under the Retain policy")
	}

	if err := getSecretErr(c); err != nil {
		t.Errorf("credentials secret was removed under the Retain policy: %v", err)
	}
	if err := c.Get(context.Background(), key, &repo); !apierrors.IsNotFound(err) {
		t.Errorf("finalizer was not removed, repository still present: %v", err)
	}
}

func TestDeletionPolicyDeleteRemovesRepository(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	})
	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	var got b2backupv1.Repository
	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	id := got.Status.RepositoryID
	if err := c.Delete(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)

	if !api.called("Delete") {
		t.Error("expected repoDelete under the Delete policy")
	}
	if _, ok := api.repos[id]; ok {
		t.Error("the BorgBase repository survived the Delete policy")
	}
}

func TestSuspendSkipsReconcile(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) { r.Spec.Suspend = true })
	r, _ := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	if api.called("Add") || api.called("FindByName") {
		t.Errorf("suspended repository still contacted BorgBase: %v", api.calls)
	}
}

func getSecretErr(c client.Client) error {
	key := types.NamespacedName{Namespace: testNS, Name: credsSecretName}
	return c.Get(context.Background(), key, &corev1.Secret{})
}

func TestRetainDeletionDoesNotNeedAPIToken(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	var repo b2backupv1.Repository
	if err := c.Get(context.Background(), key, &repo); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &repo); err != nil {
		t.Fatal(err)
	}

	if err := c.Delete(context.Background(), &corev1.Secret{
		Name: tokenName, Namespace: tokenNS,
	}); err != nil {
		t.Fatal(err)
	}

	reconcileN(t, r, 1)
	if err := c.Get(context.Background(), key, &repo); !apierrors.IsNotFound(err) {
		t.Errorf("Retain deletion was blocked by the missing token: %v", err)
	}
}

func TestDeletePolicyWithoutTokenKeepsFinalizer(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	})
	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	var got b2backupv1.Repository
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	id := got.Status.RepositoryID
	if err := c.Delete(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &corev1.Secret{
		Name: tokenName, Namespace: tokenNS,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName}); err == nil {
		t.Fatal("expected an error when the Delete policy cannot reach BorgBase")
	}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Errorf("the object should still exist while deletion cannot proceed: %v", err)
	}
	if _, ok := api.repos[id]; !ok {
		t.Error("the repository was deleted despite the failure")
	}
}

func TestSuspendReportsCondition(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) { r.Spec.Suspend = true })
	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 1)

	var got b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, b2backupv1.RepositoryConditionReady)
	if cond == nil || cond.Reason != "Suspended" {
		t.Errorf("Ready condition = %+v, want reason Suspended", cond)
	}
}

func TestNeverInventsPasswordOnceProvisioned(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	secret := getSecret(t, c, "restic-b2")
	original := string(secret.Data[KeyResticPassword])
	if original == "" {
		t.Fatal("no password was written")
	}
	if err := c.Delete(context.Background(), secret); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName}); err == nil {
		t.Fatal("expected an error rather than a newly invented password")
	}
	if err := getSecretErr(c); !apierrors.IsNotFound(err) {
		after := getSecret(t, c, "restic-b2")
		t.Errorf("a replacement Secret was written with password %q, want none",
			string(after.Data[KeyResticPassword]))
	}
}

func TestRejectsIDMismatchBetweenSpecAndStatus(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
	})
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.ExistingRepositoryID = adoptedRepoID
		usePasswordSeed(r)
		r.Status.RepositoryID = otherRepoID
	})

	r, _ := newHarness(t, api, repo, seedSecretFixture())
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName}); err == nil {
		t.Fatal("expected an error when the spec and status disagree about the repository")
	}
}

func TestSettingsDriftIsCorrected(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: testRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
		Quota: 50, QuotaEnabled: true, AlertDays: 1,
	})
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Status.RepositoryID = testRepoID
		r.Spec.QuotaGiB = ptr.To(int32(100))
		r.Spec.AlertDays = ptr.To(int32(3))
		r.Spec.AppendOnly = true
		r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: seedSecret, Key: KeyResticPassword}
	})
	seed := &corev1.Secret{
		Name: seedSecret, Namespace: testNS,
		Data: map[string][]byte{KeyResticPassword: []byte("pw")},
	}

	r, _ := newHarness(t, api, repo, seed)
	reconcileN(t, r, 1)

	if !api.called("Edit") {
		t.Fatal("settings drift did not reach BorgBase")
	}
	edit := api.lastEdit()
	if edit.Quota == nil || *edit.Quota != 107374 {
		t.Errorf("quota = %v, want 107374", edit.Quota)
	}
	if edit.AlertDays == nil || *edit.AlertDays != 3 {
		t.Errorf("alertDays = %v, want 3", edit.AlertDays)
	}
	if edit.AppendOnly == nil || !*edit.AppendOnly {
		t.Errorf("appendOnly = %v, want true", edit.AppendOnly)
	}

	if edit.QuotaEnabled == nil || !*edit.QuotaEnabled {
		t.Errorf("quotaEnabled = %v, want it sent alongside the quota", edit.QuotaEnabled)
	}
}

func TestCreatedRepositoryCarriesTheConvertedSettings(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.QuotaGiB = ptr.To(int32(100))
		r.Spec.AlertDays = ptr.To(int32(3))
		r.Spec.AppendOnly = true
	})

	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	if !api.called("Add") {
		t.Fatal("expected a repository to be created")
	}
	if api.called("Edit") {
		t.Error("a freshly created repository should already match its spec")
	}

	var created b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &created); err != nil {
		t.Fatal(err)
	}
	remote, err := api.Get(context.Background(), created.Status.RepositoryID)
	if err != nil {
		t.Fatalf("looking the created repository back up: %v", err)
	}

	if remote.Quota != 107374 {
		t.Errorf("quota = %d, want 107374", remote.Quota)
	}
	if !remote.QuotaEnabled {
		t.Error("quotaEnabled = false, want it set alongside the quota")
	}
	if remote.AlertDays != 3 {
		t.Errorf("alertDays = %d, want 3", remote.AlertDays)
	}
	if !remote.AppendOnly {
		t.Error("appendOnly = false, want true")
	}
	if created.Status.Quota != "100.0GiB" {
		t.Errorf("status.quota = %q, want %q", created.Status.Quota, "100.0GiB")
	}
}

func TestNoEditWhenSettingsMatch(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: testRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
		Quota: 107374, QuotaEnabled: true, AlertDays: 3, AppendOnly: true,
	})
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Status.RepositoryID = testRepoID
		r.Spec.QuotaGiB = ptr.To(int32(100))
		r.Spec.AlertDays = ptr.To(int32(3))
		r.Spec.AppendOnly = true
		r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: seedSecret, Key: KeyResticPassword}
	})
	seed := &corev1.Secret{
		Name: seedSecret, Namespace: testNS,
		Data: map[string][]byte{KeyResticPassword: []byte("pw")},
	}

	r, _ := newHarness(t, api, repo, seed)
	reconcileN(t, r, 2)

	if api.called("Edit") {
		t.Error("an unchanged repository was edited anyway")
	}
}

func TestRefusesToWriteUnmanagedSecret(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.SecretName = "app-credentials"
	})
	foreign := &corev1.Secret{
		Name: "app-credentials", Namespace: testNS,
		Data: map[string][]byte{"DB_PASSWORD": []byte("do not touch")},
	}

	r, c := newHarness(t, api, repo, foreign)
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName}); err == nil {
		t.Fatal("expected a refusal to write into an unmanaged Secret")
	}

	after := getSecret(t, c, "app-credentials")
	if string(after.Data["DB_PASSWORD"]) != "do not touch" {
		t.Error("the foreign Secret was modified")
	}
	if _, ok := after.Data[KeyResticPassword]; ok {
		t.Error("credentials were written into the foreign Secret")
	}
	if len(after.OwnerReferences) != 0 {
		t.Error("the operator took ownership of a Secret it did not create")
	}
}

func TestOwnerReferenceFollowsDeletionPolicy(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	})
	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	if got := metav1.GetControllerOf(getSecret(t, c, "restic-b2")); got == nil {
		t.Fatal("Delete policy should own the credentials Secret")
	}

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	var live b2backupv1.Repository
	if err := c.Get(context.Background(), key, &live); err != nil {
		t.Fatal(err)
	}
	live.Spec.DeletionPolicy = b2backupv1.DeletionPolicyRetain
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)

	if got := metav1.GetControllerOf(getSecret(t, c, "restic-b2")); got != nil {
		t.Errorf("Retain should have released the Secret, still owned by %s", got.Name)
	}
}

func TestLookupFailureLeavesStateIntact(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	before := string(getSecret(t, c, "restic-b2").Data[KeyResticPassword])
	var repo b2backupv1.Repository
	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	if err := c.Get(context.Background(), key, &repo); err != nil {
		t.Fatal(err)
	}
	id := repo.Status.RepositoryID

	api.getErr = errors.New("borgbase: gateway timeout")
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName}); err == nil {
		t.Fatal("expected the API failure to surface")
	}

	if err := c.Get(context.Background(), key, &repo); err != nil {
		t.Fatal(err)
	}
	if repo.Status.RepositoryID != id {
		t.Errorf("recorded repository ID changed to %q during an outage", repo.Status.RepositoryID)
	}
	if after := string(getSecret(t, c, "restic-b2").Data[KeyResticPassword]); after != before {
		t.Error("the password was rewritten during an API outage")
	}
}

func TestQuotaAndQuotaEnabledAreAlwaysSentTogether(t *testing.T) {
	tests := []struct {
		name          string
		specQuota     *int32
		remoteEnabled bool
		remoteQuota   int64
		wantEnabled   *bool
		wantQuota     *int64
	}{
		{
			name:      "clearing a quota still carries a value",
			specQuota: nil, remoteEnabled: true, remoteQuota: 5,
			wantEnabled: ptr.To(false), wantQuota: ptr.To(int64(5)),
		},
		{
			name:      "setting a quota sends both",
			specQuota: ptr.To(int32(10)), remoteEnabled: false,
			wantEnabled: ptr.To(true), wantQuota: ptr.To(int64(10737)),
		},
		{
			name:      "changing a quota sends both",
			specQuota: ptr.To(int32(20)), remoteEnabled: true, remoteQuota: 10737,
			wantEnabled: ptr.To(true), wantQuota: ptr.To(int64(21475)),
		},
		{
			name:      "an unchanged quota sends neither",
			specQuota: ptr.To(int32(10)), remoteEnabled: true, remoteQuota: 10737,
		},
		{
			name:      "no quota either side sends neither",
			specQuota: nil, remoteEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &b2backupv1.Repository{
				Name: resticName, Namespace: testNS,
				Spec: b2backupv1.RepositorySpec{QuotaGiB: tt.specQuota},
			}
			remote := &borgbase.Repo{QuotaEnabled: tt.remoteEnabled, Quota: tt.remoteQuota}

			remote.ID = testRepoID
			api := newFakeAPI(remote)
			r, _ := newHarness(t, api, repo)
			if _, err := r.reconcileSettings(context.Background(), repo, remote, api); err != nil {
				t.Fatalf("reconcileSettings: %v", err)
			}

			opts := api.lastEdit()
			if tt.wantEnabled == nil && tt.wantQuota == nil && api.called("Edit") {
				t.Errorf("Edit was called with %+v; nothing changed, so nothing should be sent", opts)
			}
			if !equalBoolPtr(opts.QuotaEnabled, tt.wantEnabled) {
				t.Errorf("quotaEnabled = %v, want %v", derefBool(opts.QuotaEnabled), derefBool(tt.wantEnabled))
			}
			if !equalInt64Ptr(opts.Quota, tt.wantQuota) {
				t.Errorf("quota = %v, want %v", derefInt64(opts.Quota), derefInt64(tt.wantQuota))
			}

			if (opts.QuotaEnabled == nil) != (opts.Quota == nil) {
				t.Errorf("quota and quotaEnabled must be sent together, got quota=%v enabled=%v",
					derefInt64(opts.Quota), derefBool(opts.QuotaEnabled))
			}
		})
	}
}

func equalBoolPtr(a, b *bool) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func equalInt64Ptr(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func derefBool(p *bool) any {
	if p == nil {
		return "<unset>"
	}
	return *p
}

func derefInt64(p *int64) any {
	if p == nil {
		return "<unset>"
	}
	return *p
}

func TestConflictingRepositoriesDoNotFight(t *testing.T) {
	incumbent := &b2backupv1.Repository{
		Name: resticName, Namespace: testNS,
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		Status:            b2backupv1.RepositoryStatus{RepositoryID: testRepoID, Initialized: true},
	}
	newcomer := &b2backupv1.Repository{
		Name: adoptedName, Namespace: testNS,
		CreationTimestamp: metav1.NewTime(time.Now()),
		Spec:              b2backupv1.RepositorySpec{ExistingRepositoryID: testRepoID},
	}

	api := newFakeAPI(&borgbase.Repo{
		ID: testRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
		Quota: 100, QuotaEnabled: true,
	})
	r, _ := newHarness(t, api, incumbent, newcomer)

	other, err := r.repositoryConflict(context.Background(), newcomer, testRepoID)
	if err != nil {
		t.Fatalf("repositoryConflict: %v", err)
	}
	if other != testNS+"/"+resticName {
		t.Errorf("newcomer should defer to the incumbent, got %q", other)
	}

	other, err = r.repositoryConflict(context.Background(), incumbent, testRepoID)
	if err != nil {
		t.Fatalf("repositoryConflict: %v", err)
	}
	if other != "" {
		t.Errorf("the incumbent must keep managing the repository, got conflict with %q", other)
	}
}

func TestRepositoryConflictIgnoresUnrelatedAndDeleted(t *testing.T) {
	deleting := &b2backupv1.Repository{
		Name: "old", Namespace: testNS,
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		DeletionTimestamp: ptr.To(metav1.Now()),
		Finalizers:        []string{FinalizerName},
		Status:            b2backupv1.RepositoryStatus{RepositoryID: testRepoID},
	}
	elsewhere := &b2backupv1.Repository{
		Name: "other", Namespace: "somewhere-else",
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		Status:            b2backupv1.RepositoryStatus{RepositoryID: "different"},
	}
	mine := &b2backupv1.Repository{
		Name: resticName, Namespace: testNS,
		CreationTimestamp: metav1.NewTime(time.Now()),
		Status:            b2backupv1.RepositoryStatus{RepositoryID: testRepoID},
	}

	r, _ := newHarness(t, newFakeAPI(), deleting, elsewhere, mine)

	other, err := r.repositoryConflict(context.Background(), mine, testRepoID)
	if err != nil {
		t.Fatalf("repositoryConflict: %v", err)
	}
	if other != "" {
		t.Errorf("expected no conflict, got %q", other)
	}
}

func TestRepositoryConflictIsClusterWide(t *testing.T) {
	incumbent := &b2backupv1.Repository{
		Name: resticName, Namespace: "team-a",
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		Status:            b2backupv1.RepositoryStatus{RepositoryID: testRepoID},
	}
	newcomer := &b2backupv1.Repository{
		Name: resticName, Namespace: "team-b",
		CreationTimestamp: metav1.NewTime(time.Now()),
		Spec:              b2backupv1.RepositorySpec{ExistingRepositoryID: testRepoID},
	}

	r, _ := newHarness(t, newFakeAPI(), incumbent, newcomer)

	other, err := r.repositoryConflict(context.Background(), newcomer, testRepoID)
	if err != nil {
		t.Fatalf("repositoryConflict: %v", err)
	}
	if other != "team-a/"+resticName {
		t.Errorf("expected the conflict to be found across namespaces, got %q", other)
	}
}

func TestAdoptsTheRepositoryAlreadyNamedAfterTheNamespace(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "found-token",
	})
	r, c := newHarness(t, api, repositoryFixture(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 2)

	if api.called("Add") {
		t.Fatal("a repository named after the namespace already existed, yet repoAdd was called")
	}
	if !api.called("FindByName") {
		t.Fatal("the lookup by name never happened")
	}

	var got b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.RepositoryID != adoptedRepoID {
		t.Errorf("status.repositoryID = %q, want the repository found by name (%q)",
			got.Status.RepositoryID, adoptedRepoID)
	}

	want := "rest:https://" + adoptedRepoID + ":found-token@" + adoptedRepoID + ".repo.borgbase.com"
	if url := string(getSecret(t, c, "restic-b2").Data[KeyResticRepository]); url != want {
		t.Errorf("RESTIC_REPOSITORY = %q, want %q", url, want)
	}
}

func TestSpecRepositoryNameDrivesTheLookup(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: legacyName, Format: borgbase.FormatRestic, Htpasswd: "t",
	})
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.RepositoryName = legacyName
		usePasswordSeed(r)
	})
	r, c := newHarness(t, api, repo, seedSecretFixture())
	reconcileN(t, r, 2)

	if api.called("Add") {
		t.Fatal("spec.repositoryName named an existing repository, yet a second one was created")
	}

	var got b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.RepositoryID != adoptedRepoID {
		t.Errorf("status.repositoryID = %q, want %q", got.Status.RepositoryID, adoptedRepoID)
	}
}

func TestNameLookupIsSkippedOnceAnIDIsRecorded(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
	})
	r, _ := newHarness(t, api, repositoryFixture(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 4)

	if n := api.countCalls("FindByName"); n != 1 {
		t.Errorf("FindByName was called %d times, want once; after the ID is recorded "+
			"a rename in BorgBase must not send the operator to a different repository", n)
	}
}

func TestAmbiguousNameIsNeverAdopted(t *testing.T) {
	api := newFakeAPI(
		&borgbase.Repo{ID: "aaaa1111", Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t"},
		&borgbase.Repo{ID: "bbbb2222", Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t"},
	)
	r, c := newHarness(t, api, repositoryFixture(nil))

	_, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName})
	if err == nil {
		t.Fatal("expected two repositories sharing a name to be refused, not resolved arbitrarily")
	}
	if api.called("Add") {
		t.Error("an ambiguous name fell through to repoAdd")
	}
	if err := getSecretErr(c); !apierrors.IsNotFound(err) {
		t.Error("credentials were written for a repository the operator could not identify")
	}

	var got b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.RepositoryID != "" {
		t.Errorf("status.repositoryID = %q, want it left empty", got.Status.RepositoryID)
	}
}

func TestNameMatchWithSnapshotsNeedsAPassword(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic,
		Htpasswd: "t", CurrentUsage: 8.5,
	})
	r, c := newHarness(t, api, repositoryFixture(nil))

	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName}); err == nil {
		t.Fatal("expected a refusal to invent a password for a repository found by name that holds data")
	}
	if err := getSecretErr(c); !apierrors.IsNotFound(err) {
		t.Error("a password was invented for a repository that already holds snapshots")
	}
}

func TestFinalizeToleratesARepositoryAlreadyGone(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	})
	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	var got b2backupv1.Repository
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(context.Background(), got.Status.RepositoryID); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &got); err != nil {
		t.Fatal(err)
	}

	reconcileN(t, r, 1)
	if err := c.Get(context.Background(), key, &got); !apierrors.IsNotFound(err) {
		t.Errorf("a repository already deleted in BorgBase blocked the finalizer: %v", err)
	}
}

func TestCreateAfterDeleteDoesNotReuseAnID(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	})
	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	var got b2backupv1.Repository
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	first := got.Status.RepositoryID
	if err := c.Delete(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)

	second, err := api.Add(context.Background(), borgbase.AddOptions{Name: testNS, Region: "us"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first {
		t.Errorf("the API handed out %q again after it was deleted; a fake that recycles IDs "+
			"hides a controller writing credentials for the wrong repository", first)
	}
}

func TestFakeAPIHandsOutCopies(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: testRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
	})

	got, err := api.Get(context.Background(), testRepoID)
	if err != nil {
		t.Fatal(err)
	}
	got.Format = "borg"
	got.Htpasswd = "clobbered"

	again, err := api.Get(context.Background(), testRepoID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.IsRestic() || again.Htpasswd != "t" {
		t.Errorf("a caller mutating a returned repository changed the stored one: %+v", again)
	}
}

func reconcileOnce(t *testing.T, r *RepositoryReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func initJob(t *testing.T, c client.Client) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	key := types.NamespacedName{Namespace: testNS, Name: resticName + initJobSuffix}
	if err := c.Get(context.Background(), key, &job); err != nil {
		t.Fatalf("getting the init job: %v", err)
	}
	return &job
}

func failInitJob(t *testing.T, c client.Client, at time.Time) {
	t.Helper()
	job := initJob(t, c)
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobFailed,
		Status:             corev1.ConditionTrue,
		Reason:             "BackoffLimitExceeded",
		LastTransitionTime: metav1.NewTime(at),
	}}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func initializedCondition(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	var repo b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &repo); err != nil {
		t.Fatal(err)
	}
	return apimeta.FindStatusCondition(repo.Status.Conditions, b2backupv1.RepositoryConditionInitialized)
}

func TestRequeueCadenceFromCreationToSteadyState(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))

	if res := reconcileOnce(t, r); res.RequeueAfter != wantInitPoll {
		t.Errorf("first pass RequeueAfter = %v, want %v to poll the init Job",
			res.RequeueAfter, wantInitPoll)
	}

	job := initJob(t, c)
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	if res := reconcileOnce(t, r); res.RequeueAfter != initCleanupDelay {
		t.Errorf("the pass that records success returned RequeueAfter = %v, want %v; "+
			"without it nothing comes back to delete the Job", res.RequeueAfter, initCleanupDelay)
	}
	if res := reconcileOnce(t, r); res.RequeueAfter != time.Hour {
		t.Errorf("steady state RequeueAfter = %v, want the one hour default", res.RequeueAfter)
	}
}

func TestSpecIntervalSetsTheRequeueDelay(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.Interval = &metav1.Duration{Duration: 7 * time.Minute}
	})
	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 1)

	job := initJob(t, c)
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 2)

	if res := reconcileOnce(t, r); res.RequeueAfter != 7*time.Minute {
		t.Errorf("RequeueAfter = %v, want spec.interval (7m)", res.RequeueAfter)
	}
}

func TestConflictingRepositoryStillRequeues(t *testing.T) {
	incumbent := &b2backupv1.Repository{
		Name: "incumbent", Namespace: testNS,
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		Status:            b2backupv1.RepositoryStatus{RepositoryID: adoptedRepoID},
	}
	newcomer := repositoryFixture(func(r *b2backupv1.Repository) {
		r.CreationTimestamp = metav1.NewTime(time.Now())
		r.Spec.ExistingRepositoryID = adoptedRepoID
		usePasswordSeed(r)
		r.Spec.Interval = &metav1.Duration{Duration: 3 * time.Minute}
	})
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
	})

	r, c := newHarness(t, api, incumbent, newcomer, seedSecretFixture())
	if res := reconcileOnce(t, r); res.RequeueAfter != 3*time.Minute {
		t.Errorf("RequeueAfter = %v, want spec.interval so the conflict is rechecked",
			res.RequeueAfter)
	}
	if err := getSecretErr(c); !apierrors.IsNotFound(err) {
		t.Error("the loser of a conflict wrote credentials anyway")
	}
}

func TestFailedInitJobBacksOffAndIsThenReplaced(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 1)

	failInitJob(t, c, time.Now())

	res := reconcileOnce(t, r)
	if res.RequeueAfter <= 0 || res.RequeueAfter > initRetryDelay {
		t.Errorf("RequeueAfter = %v, want the remainder of the %v backoff",
			res.RequeueAfter, initRetryDelay)
	}
	cond := initializedCondition(t, c)
	if cond == nil || cond.Reason != "InitFailed" {
		t.Fatalf("Initialized condition = %+v, want reason InitFailed", cond)
	}
	if !strings.Contains(cond.Message, "passwordSecretRef") {
		t.Errorf("Initialized message = %q, want it to name passwordSecretRef, the usual cause",
			cond.Message)
	}
	initJob(t, c)

	failInitJob(t, c, time.Now().Add(-initRetryDelay-time.Minute))

	if res := reconcileOnce(t, r); res.RequeueAfter != initRetryDelay {
		t.Errorf("RequeueAfter = %v, want %v once the backoff has elapsed",
			res.RequeueAfter, initRetryDelay)
	}
	assertWarningEvent(t, r.Recorder, "InitFailed")

	key := types.NamespacedName{Namespace: testNS, Name: resticName + initJobSuffix}
	if err := c.Get(context.Background(), key, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("the failed init Job was not deleted, so the operator can never retry: %v", err)
	}

	if res := reconcileOnce(t, r); res.RequeueAfter != wantInitPoll {
		t.Errorf("RequeueAfter = %v, want %v while the replacement Job runs",
			res.RequeueAfter, wantInitPoll)
	}
	initJob(t, c)
}

func seedSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		Name: seedSecret, Namespace: testNS,
		Data: map[string][]byte{KeyResticPassword: []byte("seeded")},
	}
}

func usePasswordSeed(r *b2backupv1.Repository) {
	r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: seedSecret, Key: KeyResticPassword}
}

func repositoryStatus(t *testing.T, c client.Client) b2backupv1.RepositoryStatus {
	t.Helper()
	var repo b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &repo); err != nil {
		t.Fatal(err)
	}
	return repo.Status
}

func TestNameMatchIsReportedAsAdopted(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
	})
	r, c := newHarness(t, api, repositoryFixture(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 1)

	if !repositoryStatus(t, c).Adopted {
		t.Error("a repository matched by name is an existing repository this resource did not " +
			"create, so status.adopted must be true")
	}
}

func TestCreatedRepositoryIsNotReportedAsAdopted(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 1)

	if repositoryStatus(t, c).Adopted {
		t.Error("status.adopted is set on a repository the operator created")
	}
}

func TestAdoptionSurvivesTheRecordedIDPath(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
	})
	r, c := newHarness(t, api, repositoryFixture(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 3)

	if n := api.countCalls("FindByName"); n != 1 {
		t.Fatalf("FindByName was called %d times, want once; the later passes must take "+
			"the recorded-ID path this test is about", n)
	}
	if !repositoryStatus(t, c).Adopted {
		t.Error("status.adopted flapped back to false on a pass that resolved by recorded ID")
	}
}

func TestEmptyLookingNameMatchStillNeedsAPassword(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic,
		Htpasswd: "t", CurrentUsage: 0,
	})
	r, c := newHarness(t, api, repositoryFixture(nil))

	_, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName})
	if err == nil {
		t.Fatal("a repository adopted by name got a freshly generated password; BorgBase usage " +
			"is not live, so it cannot be the only thing keeping the operator off an existing repository")
	}
	if !strings.Contains(err.Error(), "already been provisioned") {
		t.Errorf("error = %v, want the guard that names passwordSecretRef", err)
	}
	if err := getSecretErr(c); !apierrors.IsNotFound(err) {
		t.Errorf("credentials were written for an adopted repository: %v", err)
	}
}

func TestAnInterruptedCreatePassStillRecordsTheRepositoryID(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	})
	fail := true
	r, c := newHarnessWithInterceptor(t, api, failRepositoryList(&fail), repo)

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: resticName,
	}); err == nil {
		t.Fatal("expected the interrupted pass to fail")
	}
	if !api.called("Add") {
		t.Fatalf("the pass never created a repository: %v", api.calls)
	}

	var got b2backupv1.Repository
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	id := got.Status.RepositoryID
	if id == "" {
		t.Fatal("a repository was created but its ID was never recorded; " +
			"deleting this Repository now removes the finalizer without deleting " +
			"the live, still-billed BorgBase repository")
	}

	if err := c.Delete(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)

	if _, err := api.Get(context.Background(), id); !errors.Is(err, borgbase.ErrNotFound) {
		t.Errorf("BorgBase repository %s outlived its Repository: %v", id, err)
	}
}

func TestAnInterruptedCreatePassDoesNotWedgeOnAdoption(t *testing.T) {
	api := newFakeAPI()
	fail := true
	r, c := newHarnessWithInterceptor(t, api, failRepositoryList(&fail), repositoryFixture(nil))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: resticName,
	}); err == nil {
		t.Fatal("expected the interrupted pass to fail")
	}

	fail = false
	reconcileN(t, r, 1)

	var got b2backupv1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Adopted {
		t.Error("the retry adopted the repository this operator had just created; " +
			"that makes reconcileSecret refuse to generate a password and wedges a fresh install")
	}
	if api.countCalls("Add") != 1 {
		t.Errorf("Add called %d times, want exactly one", api.countCalls("Add"))
	}
	if got.Status.SecretName == "" {
		t.Error("the retry never wrote credentials")
	}
}

func TestTheLoserOfAConflictNeverRecordsTheRepositoryID(t *testing.T) {
	incumbent := &b2backupv1.Repository{
		Name: "incumbent", Namespace: testNS,
		CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		Status:            b2backupv1.RepositoryStatus{RepositoryID: adoptedRepoID, Initialized: true},
	}
	newcomer := repositoryFixture(func(r *b2backupv1.Repository) {
		r.CreationTimestamp = metav1.NewTime(time.Now())
		r.Spec.ExistingRepositoryID = adoptedRepoID
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
		usePasswordSeed(r)
	})
	api := newFakeAPI(&borgbase.Repo{
		ID: adoptedRepoID, Name: testNS, Format: borgbase.FormatRestic, Htpasswd: "t",
	})

	r, c := newHarness(t, api, incumbent, newcomer, seedSecretFixture())
	reconcileN(t, r, 2)

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	var got b2backupv1.Repository
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.RepositoryID != "" {
		t.Fatalf("the loser of a conflict recorded status.repositoryID = %q; "+
			"both Repositories now claim the same repository and deleting the loser "+
			"destroys the incumbent's live repository", got.Status.RepositoryID)
	}

	if err := c.Delete(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)

	if _, err := api.Get(context.Background(), adoptedRepoID); err != nil {
		t.Errorf("deleting the loser destroyed the incumbent's repository: %v", err)
	}
	if api.called("Delete") {
		t.Error("the loser of a conflict called repoDelete")
	}
}

func recordedEvents(t *testing.T, r *RepositoryReconciler) []string {
	t.Helper()
	rec, ok := r.Recorder.(*events.FakeRecorder)
	if !ok {
		t.Fatalf("recorder is %T, want *events.FakeRecorder", r.Recorder)
	}
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// provisionForDeletion runs a Repository to steady state under the Delete policy
// and returns it mid-deletion, with the finalizer still attached.
func provisionForDeletion(
	t *testing.T, r *RepositoryReconciler, c client.Client,
) *b2backupv1.Repository {
	t.Helper()
	reconcileN(t, r, 2)

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	var repo b2backupv1.Repository
	if err := c.Get(context.Background(), key, &repo); err != nil {
		t.Fatal(err)
	}
	if repo.Status.RepositoryID == "" {
		t.Fatal("no repository was provisioned")
	}
	if err := c.Delete(context.Background(), &repo); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key, &repo); err != nil {
		t.Fatal(err)
	}
	return &repo
}

func TestFinalizeNeverTrustsANotFoundLookingDeleteError(t *testing.T) {
	// Every one of these is a plausible repoDelete failure that leaves the
	// repository alive, and every one matches the ErrNotFound heuristic.
	for _, message := range []string{
		"Cannot delete repository: backup job not found",
		"repository quota not found",
		"SSH key for repository not found",
	} {
		api := newFakeAPI()
		r, c := newHarness(t, api, repositoryFixture(func(repo *b2backupv1.Repository) {
			repo.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
		}))
		repo := provisionForDeletion(t, r, c)
		id := repo.Status.RepositoryID
		api.deleteErr = fmt.Errorf("%w: %s", borgbase.ErrNotFound, message)

		key := types.NamespacedName{Namespace: testNS, Name: resticName}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			Namespace: testNS, Name: resticName,
		}); err == nil {
			t.Errorf("%q: finalize succeeded while the repository was still there", message)
		}

		var got b2backupv1.Repository
		if err := c.Get(context.Background(), key, &got); err != nil {
			t.Fatalf("%q: the Repository was released: %v", message, err)
		}
		if !controllerutil.ContainsFinalizer(&got, FinalizerName) {
			t.Errorf("%q: the finalizer was removed while repository %s was still live", message, id)
		}
		if _, err := api.Get(context.Background(), id); err != nil {
			t.Errorf("%q: repository %s vanished: %v", message, id, err)
		}
		for _, e := range recordedEvents(t, r) {
			if strings.Contains(e, "and all its snapshots") {
				t.Errorf("%q: reported a deletion that never happened: %s", message, e)
			}
		}
	}
}

func TestFinalizeReleasesARepositoryTheProbeSaysIsGone(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(func(repo *b2backupv1.Repository) {
		repo.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	repo := provisionForDeletion(t, r, c)
	id := repo.Status.RepositoryID

	// The repoDelete landed, the finalizer patch did not, and the retry now
	// hits a repository that is genuinely gone. Failing closed here would wedge
	// the finalizer and block namespace termination.
	if err := api.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	api.deleteErr = errors.New("borgbase: repoDelete reported ok=false for repository " + id)
	_ = recordedEvents(t, r)

	reconcileN(t, r, 1)

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	if err := c.Get(context.Background(), key, &b2backupv1.Repository{}); !apierrors.IsNotFound(err) {
		t.Errorf("a repository already gone from BorgBase wedged the finalizer: %v", err)
	}

	var unconfirmed bool
	for _, e := range recordedEvents(t, r) {
		if strings.Contains(e, "and all its snapshots") {
			t.Errorf("claimed a confirmed deletion after repoDelete failed: %s", e)
		}
		if strings.Contains(e, "RepositoryDeleteUnconfirmed") {
			unconfirmed = true
		}
	}
	if !unconfirmed {
		t.Error("no RepositoryDeleteUnconfirmed event; the operator reported nothing unusual")
	}
}

func TestForceFinalizeAnnotationReleasesAnUndeletableRepository(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(func(repo *b2backupv1.Repository) {
		repo.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	repo := provisionForDeletion(t, r, c)
	id := repo.Status.RepositoryID
	api.deleteErr = errors.New("borgbase: token lacks repoDelete scope")

	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: resticName,
	}); err == nil {
		t.Fatal("expected the undeletable repository to hold the finalizer")
	}

	var got b2backupv1.Repository
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	got.Annotations = map[string]string{b2backupv1.AnnotationForceFinalize: "true"}
	if err := c.Update(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	_ = recordedEvents(t, r)

	reconcileN(t, r, 1)

	if err := c.Get(context.Background(), key, &b2backupv1.Repository{}); !apierrors.IsNotFound(err) {
		t.Errorf("the annotation did not release the Repository: %v", err)
	}
	var orphaned bool
	for _, e := range recordedEvents(t, r) {
		if strings.Contains(e, "and all its snapshots") {
			t.Errorf("claimed a deletion that never happened: %s", e)
		}
		if strings.Contains(e, "RepositoryOrphaned") {
			orphaned = true
		}
	}
	if !orphaned {
		t.Error("no RepositoryOrphaned event warning that a billed repository was left behind")
	}
	if _, err := api.Get(context.Background(), id); err != nil {
		t.Errorf("repository %s should have been left alone: %v", id, err)
	}
}
