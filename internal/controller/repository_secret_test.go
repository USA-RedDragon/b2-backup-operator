package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
)

func TestSecretFinalizerProtectsAnUnseededPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		policy    b2backupv1.DeletionPolicy
		seed      *corev1.Secret
		seedRef   bool
		protected bool
	}{
		{"a generated password under Retain", b2backupv1.DeletionPolicyRetain, nil, false, true},
		{"a seeded password under Retain", b2backupv1.DeletionPolicyRetain, seedSecretFixture(), true, false},
		{"a generated password under Delete", b2backupv1.DeletionPolicyDelete, nil, false, true},
		{"a seeded password under Delete", b2backupv1.DeletionPolicyDelete, seedSecretFixture(), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			objs := []client.Object{bucketRepo(func(r *b2backupv1.Repository) {
				r.Spec.DeletionPolicy = tt.policy
				if tt.seedRef {
					usePasswordSeed(r)
				}
			})}
			if tt.seed != nil {
				objs = append(objs, tt.seed)
			}
			r, c := newHarness(t, f, objs...)
			reconcileN(t, r, 2)
			has := slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer)
			if has != tt.protected {
				t.Errorf("Secret finalizer = %t, want %t", has, tt.protected)
			}
		})
	}

	t.Run("finalize keeps it on a generated password", func(t *testing.T) {
		t.Parallel()
		f := newFakeB2(t)
		r, c := newHarness(t, f, bucketRepo(nil))
		reconcileN(t, r, 2)
		deleteRepo(t, c)
		reconcileUntilGone(t, r, c)
		if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
			t.Error("the only copy of a generated password lost its protection")
		}
		if !hasEvent(recordedEvents(t, r), "PasswordNotArchived") {
			t.Error("no PasswordNotArchived warning")
		}
	})

	t.Run("finalize releases it once the seed holds the password", func(t *testing.T) {
		t.Parallel()
		f := newFakeB2(t)
		r, c := newHarness(t, f, bucketRepo(nil), seedSecretFixture())
		reconcileN(t, r, 2)
		password := getSecret(t, c, credsSecretName).Data[KeyResticPassword]
		seed := &corev1.Secret{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: seedSecret}, seed); err != nil {
			t.Fatal(err)
		}
		seed.Data[KeyResticPassword] = password
		if err := c.Update(context.Background(), seed); err != nil {
			t.Fatal(err)
		}
		repo := getRepo(t, c)
		usePasswordSeed(repo)
		if err := c.Update(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
		deleteRepo(t, c)
		reconcileUntilGone(t, r, c)
		if err := getSecretErr(c); !apierrors.IsNotFound(err) {
			t.Errorf("a password preserved in its seed kept the Secret after its key was deleted: %v", err)
		}
	})

	t.Run("a stale seed does not count", func(t *testing.T) {
		t.Parallel()
		f := newFakeB2(t)
		bucket := f.AddBucket(ownBucket)
		f.PutFile(bucket.BucketID, "config", 1)
		r, c := newHarness(t, f, bucketRepo(usePasswordSeed), seedSecretFixture())
		reconcileN(t, r, 1)
		secret := getSecret(t, c, credsSecretName)
		secret.Data[KeyResticPassword] = []byte("rotated-by-b2bk")
		if err := c.Update(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
		reconcileN(t, r, 1)
		if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
			t.Error("a seed holding another password was taken as a copy of this one")
		}
	})
}

func TestAMissingSeedNeverDropsTheProtection(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 2)
	repo := getRepo(t, c)
	repo.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: "not-applied-yet", Key: KeyResticPassword}
	if err := c.Update(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)
	if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Fatal("a seed that does not exist was taken as a copy of a generated password")
	}
	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)
	if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Error("finalize released the only copy because the seed it never confirmed is missing")
	}
}

func TestASeedGoneWithItsNamespaceStillCounts(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	bucket := f.AddBucket(ownBucket)
	f.PutFile(bucket.BucketID, "config", 1)
	r, c := newHarness(t, f, bucketRepo(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 2)
	if slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Fatal("precondition: a matching seed needs no finalizer")
	}
	seed := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: seedSecret}, seed); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)
	if slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Error("a seed confirmed before its namespace went was not trusted at finalize")
	}
}

func TestTwoRepositoriesNeverShareACredentialsSecret(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	older := prefixRepo(func(r *b2backupv1.Repository) {
		r.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	})
	newer := prefixRepo(func(r *b2backupv1.Repository) {
		r.Name, r.UID = "scratch", "0b1c2d3e-0000-4000-8000-0000000000bb"
		r.CreationTimestamp = metav1.NewTime(time.Now())
		r.Spec.Path = "restic/scratch"
		r.Spec.SecretName = credsSecretName
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	})
	r, c := newHarness(t, f, older, newer)
	reconcileN(t, r, 2)
	before := getSecret(t, c, credsSecretName)
	keys := len(ownKeys(f))

	scratch := ctrl.Request{Namespace: testNS, Name: "scratch"}
	for range 3 {
		if _, err := r.Reconcile(context.Background(), scratch); err != nil {
			t.Fatal(err)
		}
	}
	var got b2backupv1.Repository
	if err := c.Get(context.Background(), scratch.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, b2backupv1.RepositoryConditionReady); cond == nil ||
		cond.Reason != reasonConflict || !strings.Contains(cond.Message, credsSecretName) {
		t.Errorf("Ready = %+v, want a conflict naming the shared Secret", cond)
	}
	after := getSecret(t, c, credsSecretName)
	if string(after.Data[KeyResticRepository]) != string(before.Data[KeyResticRepository]) ||
		metav1.GetControllerOf(after) != nil || !slices.Contains(after.Finalizers, SecretFinalizer) {
		t.Errorf("the loser rewrote the winner's Secret: %s owner %v finalizers %v",
			after.Data[KeyResticRepository], metav1.GetControllerOf(after), after.Finalizers)
	}
	if n := len(ownKeys(f)); n != keys {
		t.Errorf("the loser created keys: %d, want %d", n, keys)
	}
}

func TestARetainedPasswordIsNeverTakenOver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
	}{
		{"by a Repository at another path", "restic/elsewhere"},
		{"by a new Repository at the same path", myappPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			f.AddBucket(sharedBucket)
			r, c := newHarness(t, f, prefixRepo(nil))
			reconcileN(t, r, 1)
			succeedInitJob(t, c)
			reconcileN(t, r, 1)
			deleteRepo(t, c)
			reconcileUntilGone(t, r, c)
			retained := getSecret(t, c, credsSecretName)
			if !slices.Contains(retained.Finalizers, SecretFinalizer) {
				t.Fatal("precondition: the retained Secret should keep its finalizer")
			}

			successor := prefixRepo(func(r *b2backupv1.Repository) {
				r.UID = "0b1c2d3e-0000-4000-8000-0000000000cc"
				r.Spec.Path = tt.path
				r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
			})
			if err := c.Create(context.Background(), successor); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				_ = reconcileErr(r)
			}
			if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond == nil ||
				cond.Reason != reasonRetainedPassword {
				t.Errorf("Ready = %+v, want %s", cond, reasonRetainedPassword)
			}
			assertSecretUntouched(t, getSecret(t, c, credsSecretName), retained)

			deleteRepo(t, c)
			reconcileUntilGone(t, r, c)
			assertSecretUntouched(t, getSecret(t, c, credsSecretName), retained)
		})
	}
}

func assertSecretUntouched(t *testing.T, got, want *corev1.Secret) {
	t.Helper()
	for _, key := range SecretKeys {
		if string(got.Data[key]) != string(want.Data[key]) {
			t.Errorf("%s changed from %q to %q", key, want.Data[key], got.Data[key])
		}
	}
	if !slices.Contains(got.Finalizers, SecretFinalizer) {
		t.Error("the finalizer protecting a retained password was removed")
	}
	if owner := metav1.GetControllerOf(got); owner != nil {
		t.Errorf("the retained Secret is now controlled by %s", owner.Name)
	}
}

func TestAReleasedSecretIsReusedOnceItsFinalizerIsRemoved(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	r, c := newHarness(t, f, prefixRepo(nil))
	reconcileN(t, r, 1)
	succeedInitJob(t, c)
	reconcileN(t, r, 1)
	password := string(getSecret(t, c, credsSecretName).Data[KeyResticPassword])
	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)

	if err := c.Create(context.Background(), prefixRepo(func(r *b2backupv1.Repository) {
		r.UID = "0b1c2d3e-0000-4000-8000-0000000000cc"
	})); err != nil {
		t.Fatal(err)
	}
	if err := reconcileErr(r); err == nil {
		t.Fatal("precondition: the retained Secret should block the new Repository")
	}
	secret := getSecret(t, c, credsSecretName)
	secret.Finalizers = nil
	if err := c.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 2)

	secret = getSecret(t, c, credsSecretName)
	if got := string(secret.Data[KeyResticPassword]); got != password {
		t.Errorf("password = %q, want the archived one", got)
	}
	if !slices.Contains(secret.Finalizers, SecretFinalizer) {
		t.Error("the reused generated password is not protected again")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason == reasonRetainedPassword {
		t.Errorf("Ready = %+v after the finalizer was removed by hand", cond)
	}
}

func TestAnUnattributedRetainedSecretForAnotherPathIsNotTakenOver(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	legacy := &corev1.Secret{
		Name: credsSecretName, Namespace: testNS,
		Labels:     map[string]string{labelManagedBy: managedByValue},
		Finalizers: []string{SecretFinalizer},
		Data: map[string][]byte{
			KeyResticRepository: []byte("s3:https://s3.us-east-005.backblazeb2.com/" + sharedBucket + "/restic/old"),
			KeyResticPassword:   []byte("only-copy"),
		},
	}
	r, c := newHarness(t, f, prefixRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}), legacy.DeepCopy())
	for range 2 {
		_ = reconcileErr(r)
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != reasonRetainedPassword {
		t.Errorf("Ready = %+v, want %s", cond, reasonRetainedPassword)
	}
	if getRepo(t, c).Status.BucketID != "" {
		t.Error("a blocked Repository recorded a location it could delete")
	}
	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)
	got := getSecret(t, c, credsSecretName)
	assertSecretUntouched(t, got, legacy)
	if len(ownKeys(f)) != 0 {
		t.Error("a blocked Repository created an application key")
	}
}

func TestALeftoverSecretForAnotherRepositoryNeverSuppliesThePassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		seeded bool
		adopt  bool
	}{
		{"a seeded fresh path", true, false},
		{"a seeded adoption", true, true},
		{"an unseeded fresh path", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			bucket := f.AddBucket(sharedBucket)
			if tt.adopt {
				f.PutFile(bucket.BucketID, myappPath+"/config", 1)
			}
			leftover := &corev1.Secret{
				Name: credsSecretName, Namespace: testNS,
				Labels: map[string]string{labelManagedBy: managedByValue},
				Data: map[string][]byte{
					KeyResticRepository: []byte("s3:https://s3.us-east-005.backblazeb2.com/" + sharedBucket + "/restic/old"),
					KeyResticPassword:   []byte("old-repository-password"),
				},
			}
			objs := []client.Object{leftover}
			repo := prefixRepo(nil)
			if tt.seeded {
				usePasswordSeed(repo)
				objs = append(objs, seedSecretFixture())
			}
			r, c := newHarness(t, f, append(objs, repo)...)
			reconcileN(t, r, 2)

			got := string(getSecret(t, c, credsSecretName).Data[KeyResticPassword])
			if got == "old-repository-password" {
				t.Fatal("the password of another repository was used for this one")
			}
			if want := string(seedSecretFixture().Data[KeyResticPassword]); tt.seeded && got != want {
				t.Errorf("password = %q, want the seed's", got)
			}
		})
	}
}

func TestASeedThatIsTheCredentialsSecretPreservesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 2)
	if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Fatal("precondition: a generated password is protected")
	}
	repo := getRepo(t, c)
	repo.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: credsSecretName, Key: KeyResticPassword}
	if err := c.Update(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if err := reconcileErr(r); err == nil {
		t.Error("a passwordSecretRef naming the credentials Secret itself was accepted")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != reasonSeedIsCredentials {
		t.Errorf("Ready = %+v, want %s", cond, reasonSeedIsCredentials)
	}
	if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Fatal("the Secret was taken as a copy of its own password and lost its finalizer")
	}
	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)
	if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Error("finalize took the Secret as a copy of its own password")
	}
}

func TestNotOwningTheSecretOutlastsEveryOtherFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		fail   func(*testing.T, *RepositoryReconciler, client.Client, *b2fake.Server)
		reason string
	}{
		{"a B2 outage", func(_ *testing.T, _ *RepositoryReconciler, _ client.Client, f *b2fake.Server) {
			f.Fail("b2_list_buckets", b2fake.Failure{Status: 503, Code: b2.CodeServiceUnavailable, Times: 1000})
		}, "ReconcileFailed"},
		{"suspension", func(t *testing.T, _ *RepositoryReconciler, c client.Client, _ *b2fake.Server) {
			repo := getRepo(t, c)
			repo.Spec.Suspend = true
			if err := c.Update(context.Background(), repo); err != nil {
				t.Fatal(err)
			}
		}, "Suspended"},
		{"missing credentials", func(_ *testing.T, r *RepositoryReconciler, _ client.Client, _ *b2fake.Server) {
			r.DefaultCredentialsSecret.Name = missingSecret
		}, "CredentialsUnavailable"},
		{"rejected credentials", func(t *testing.T, r *RepositoryReconciler, c client.Client, _ *b2fake.Server) {
			if err := c.Create(context.Background(), &corev1.Secret{
				Name: "bad-b2", Namespace: credsNS,
				Data: map[string][]byte{
					b2backupv1.CredentialsKeyID: []byte(masterKeyID), b2backupv1.CredentialsKey: []byte("wrong"),
				},
			}); err != nil {
				t.Fatal(err)
			}
			r.DefaultCredentialsSecret.Name = "bad-b2"
		}, "CredentialsRejected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			f.AddBucket(sharedBucket)
			r, c := newHarness(t, f, prefixRepo(usePasswordSeed), seedSecretFixture())
			reconcileN(t, r, 1)
			succeedInitJob(t, c)
			reconcileN(t, r, 1)
			if why := RepositoryNotOwn(getRepo(t, c)); why != "" {
				t.Fatalf("precondition: a provisioned Repository is its own, got %q", why)
			}

			secret := getSecret(t, c, credsSecretName)
			delete(secret.Labels, labelManagedBy)
			if err := c.Update(context.Background(), secret); err != nil {
				t.Fatal(err)
			}
			_ = reconcileErr(r)
			if RepositoryNotOwn(getRepo(t, c)) == "" {
				t.Fatal("precondition: an unmanaged Secret is not the Repository's own")
			}

			tt.fail(t, r, c, f)
			_ = reconcileErr(r)
			if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != tt.reason {
				t.Fatalf("precondition: Ready = %+v, want %s", cond, tt.reason)
			}
			if RepositoryNotOwn(getRepo(t, c)) == "" {
				t.Errorf("%s replaced Ready and the Repository was taken as its own again", tt.reason)
			}
		})
	}
}

func TestOwnershipIsRegainedOnlyByPassingTheChecks(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	r, c := newHarness(t, f, prefixRepo(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 1)
	secret := getSecret(t, c, credsSecretName)
	delete(secret.Labels, labelManagedBy)
	if err := c.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	_ = reconcileErr(r)
	if cond := condition(t, c, b2backupv1.RepositoryConditionOwned); cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != reasonSecretNotManaged {
		t.Fatalf("Owned = %+v, want False %s", cond, reasonSecretNotManaged)
	}
	secret = getSecret(t, c, credsSecretName)
	secret.Labels = map[string]string{labelManagedBy: managedByValue}
	if err := c.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)
	if why := RepositoryNotOwn(getRepo(t, c)); why != "" {
		t.Errorf("the Repository passed every check and is still not its own: %s", why)
	}
}

func TestAnotherRepositorysCredentialsSecretIsNoSeed(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 2)
	password := getSecret(t, c, credsSecretName).Data[KeyResticPassword]
	if err := c.Create(context.Background(), &corev1.Secret{
		Name: "sibling-credentials", Namespace: testNS,
		Labels: map[string]string{labelManagedBy: managedByValue},
		Data:   map[string][]byte{KeyResticPassword: password},
	}); err != nil {
		t.Fatal(err)
	}
	repo := getRepo(t, c)
	repo.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: "sibling-credentials", Key: KeyResticPassword}
	if err := c.Update(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	_ = reconcileErr(r)
	if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Error("a Secret the operator manages for another Repository was taken as a lasting copy of the password")
	}
}
