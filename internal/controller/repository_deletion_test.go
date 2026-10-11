package controller

import (
	"context"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
)

func deleteRepo(t *testing.T, c client.Client) {
	t.Helper()
	if err := c.Delete(context.Background(), getRepo(t, c)); err != nil {
		t.Fatal(err)
	}
}

func annotateRepo(t *testing.T, c client.Client, key, value string) {
	t.Helper()
	repo := getRepo(t, c)
	if repo.Annotations == nil {
		repo.Annotations = map[string]string{}
	}
	repo.Annotations[key] = value
	if err := c.Update(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
}

func TestRetainFinalizeAlwaysReleasesTheSecret(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	reconcileN(t, r, 2)
	if metav1.GetControllerOf(getSecret(t, c, credsSecretName)) == nil {
		t.Fatal("precondition: the Delete policy should own the Secret")
	}

	repo := getRepo(t, c)
	repo.Spec.DeletionPolicy = b2backupv1.DeletionPolicyRetain
	if err := c.Update(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	r.DefaultCredentialsSecret.Name = missingSecret
	_ = reconcileErr(r)
	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)

	if owner := metav1.GetControllerOf(getSecret(t, c, credsSecretName)); owner != nil {
		t.Errorf("Retain finalize left the Secret controlled by %s; garbage collection deletes the only password",
			owner.Name)
	}
	if f.Bucket(ownBucket) == nil {
		t.Error("Retain deleted the bucket")
	}
}

func TestTamperedStatusNeverWidensDeletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*b2backupv1.RepositoryStatus, string)
	}{
		{"an empty path", func(s *b2backupv1.RepositoryStatus, _ string) { s.Path = "" }},
		{"a parent path", func(s *b2backupv1.RepositoryStatus, _ string) { s.Path = resticName }},
		{"another bucket name", func(s *b2backupv1.RepositoryStatus, _ string) { s.BucketName = otherBucket }},
		{"another bucket ID", func(s *b2backupv1.RepositoryStatus, other string) { s.BucketID = other }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			bucket := f.AddBucket(sharedBucket)
			other := f.AddBucket(otherBucket)
			r, c := newHarness(t, f, prefixRepo(func(r *b2backupv1.Repository) {
				r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
			}))
			reconcileN(t, r, 2)
			for _, name := range []string{"restic/myapp/config", "restic/otherapp/config", "restic/otherapp/data/00/aa"} {
				f.PutFile(bucket.BucketID, name, 1)
			}
			f.PutFile(other.BucketID, "config", 1)

			repo := getRepo(t, c)
			base := repo.DeepCopy()
			tt.mutate(&repo.Status, other.BucketID)
			if err := c.Status().Patch(context.Background(), repo, client.MergeFrom(base)); err != nil {
				t.Fatal(err)
			}
			deleteRepo(t, c)
			reconcileN(t, r, 3)

			if !slices.Contains(getRepo(t, c).Finalizers, FinalizerName) {
				t.Fatal("the finalizer was released although the deletion scope did not match")
			}
			if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != reasonScope {
				t.Errorf("Ready = %+v, want %s", cond, reasonScope)
			}
			if n := len(f.FileNames(bucket.BucketID)); n != 3 {
				t.Errorf("%d files left in the shared bucket, want all 3", n)
			}
			if n := len(f.FileNames(other.BucketID)); n != 1 {
				t.Error("another bucket's files were deleted")
			}

			annotateRepo(t, c, b2backupv1.AnnotationForceFinalize, "true")
			reconcileUntilGone(t, r, c)
			if n := len(f.FileNames(bucket.BucketID)); n != 3 {
				t.Errorf("force-finalize deleted data: %d files left, want 3", n)
			}
			if !hasEvent(recordedEvents(t, r), "RepositoryOrphaned") {
				t.Error("no RepositoryOrphaned event")
			}
		})
	}
}

func TestDeleteTouchesOnlyResticLayout(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	provisionForDeletion(t, r, c)
	bucket := f.Bucket(ownBucket)
	for _, name := range []string{"config", "keys/k", "data/00/aa", "snapshots/s", notesFile, "restic/other/config"} {
		f.PutFile(bucket.BucketID, name, 1)
	}

	reconcileUntilGone(t, r, c)
	if f.Bucket(ownBucket) == nil {
		t.Fatal("a bucket still holding other files was deleted")
	}
	got := slices.Sorted(slices.Values(f.FileNames(bucket.BucketID)))
	if want := []string{notesFile, "restic/other/config"}; !slices.Equal(got, want) {
		t.Errorf("remaining = %v, want %v", got, want)
	}
	events := recordedEvents(t, r)
	if !hasEvent(events, "BucketRetained") || !hasEvent(events, "OtherFilesRetained") {
		t.Errorf("events = %v, want BucketRetained and OtherFilesRetained", events)
	}
}

func TestReinitAnnotationRestartsInitialization(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(nil))
	reconcileN(t, r, 1)
	failInitJob(t, c, time.Now())
	reconcileN(t, r, 1)

	at := time.Now().UTC().Truncate(time.Second)
	annotateRepo(t, c, b2backupv1.AnnotationReinitAt, at.Format(time.RFC3339))
	reconcileN(t, r, 1)

	repo := getRepo(t, c)
	if repo.Status.LastReinitTime == nil || !repo.Status.LastReinitTime.Time.Equal(at) {
		t.Errorf("lastReinitTime = %v, want %v", repo.Status.LastReinitTime, at)
	}
	if !hasEvent(recordedEvents(t, r), "Reinitializing") {
		t.Error("no Reinitializing event")
	}
	job := initJob(t, c)
	if jobFailedAt(job) != nil {
		t.Error("the failed init Job was not replaced")
	}

	succeedInitJob(t, c)
	reconcileN(t, r, 2)
	if !getRepo(t, c).Status.Initialized {
		t.Fatal("not initialized after the new Job succeeded")
	}
	reconcileN(t, r, 1)
	if !getRepo(t, c).Status.Initialized {
		t.Error("an annotation already acted on reset status.initialized again")
	}
}

func TestDeletePolicyKeepsThePasswordUntilTheDataIsGone(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	r, c := newHarness(t, f, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	provisionForDeletion(t, r, c)
	bucket := f.Bucket(ownBucket)
	f.PutFile(bucket.BucketID, "config", 1)
	password := string(getSecret(t, c, credsSecretName).Data[KeyResticPassword])

	if err := c.Delete(context.Background(), getSecret(t, c, credsSecretName)); err != nil {
		t.Fatal(err)
	}
	r.DefaultCredentialsSecret.Name = missingSecret
	for range 3 {
		if err := reconcileErr(r); err == nil {
			t.Fatal("finalize succeeded without credentials")
		}
	}
	if err := getSecretErr(c); err != nil {
		t.Fatalf("the password went while the data it encrypts is still in B2: %v", err)
	}
	if got := string(getSecret(t, c, credsSecretName).Data[KeyResticPassword]); got != password {
		t.Fatalf("password = %q, want %q", got, password)
	}

	r.DefaultCredentialsSecret.Name = credsName
	reconcileUntilGone(t, r, c)
	if f.Bucket(ownBucket) != nil {
		t.Error("the bucket survived")
	}
	if err := getSecretErr(c); !apierrors.IsNotFound(err) {
		t.Errorf("the Secret outlived the data it protected: %v", err)
	}
}

const (
	otherAccountID = "otheraccount"
	otherKeyID     = "0099bb000000000000000000bb"
	otherKey       = "K009otherkeysecret"
)

func pointAtAnotherAccount(t *testing.T, r *RepositoryReconciler, c client.Client) {
	t.Helper()
	other := b2fake.NewAccount(otherAccountID, otherKeyID, otherKey)
	other.Logf = t.Logf
	srv := httptest.NewServer(other)
	t.Cleanup(srv.Close)
	same := r.NewAPI
	r.NewAPI = func(keyID, key string) b2.API {
		if keyID != otherKeyID {
			return same(keyID, key)
		}
		cl := b2.NewClient(keyID, key)
		cl.AuthURL = srv.URL
		cl.Sleep = func(context.Context, time.Duration) error { return nil }
		return cl
	}
	if err := c.Create(context.Background(), &corev1.Secret{
		Name: teamSecret, Namespace: testNS,
		Data: map[string][]byte{
			b2backupv1.CredentialsKeyID: []byte(otherKeyID),
			b2backupv1.CredentialsKey:   []byte(otherKey),
		},
	}); err != nil {
		t.Fatal(err)
	}
	repo := getRepo(t, c)
	repo.Spec.CredentialsSecretRef = &corev1.LocalObjectReference{Name: teamSecret}
	if err := c.Update(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
}

func TestABucketMissingFromAnotherAccountIsNotTakenAsGone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		legacy bool
	}{
		{"with the account recorded", false},
		{"with no account recorded", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			r, c := newHarness(t, f, prefixRepo(func(r *b2backupv1.Repository) {
				r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
			}))
			bucket := f.AddBucket(sharedBucket)
			reconcileN(t, r, 2)
			f.PutFile(bucket.BucketID, myappPath+"/config", 1)
			if tt.legacy {
				repo := getRepo(t, c)
				base := repo.DeepCopy()
				repo.Status.AccountID = ""
				if err := c.Status().Patch(context.Background(), repo, client.MergeFrom(base)); err != nil {
					t.Fatal(err)
				}
			}
			pointAtAnotherAccount(t, r, c)
			deleteRepo(t, c)
			for range 3 {
				_ = reconcileErr(r)
			}

			if !slices.Contains(getRepo(t, c).Finalizers, FinalizerName) {
				t.Fatal("the finalizer was released although the bucket was only missing from another account")
			}
			if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != reasonAccountMismatch {
				t.Errorf("Ready = %+v, want %s", cond, reasonAccountMismatch)
			}
			if err := getSecretErr(c); err != nil {
				t.Errorf("the password went with the data still in B2: %v", err)
			}
			if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
				t.Error("the Secret was released for garbage collection")
			}
			if n := len(f.FileNames(bucket.BucketID)); n != 1 {
				t.Errorf("%d files left, want 1", n)
			}
		})
	}
}

func TestTheAccountIsRecordedAndChecked(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	r, c := newHarness(t, f, prefixRepo(nil))
	reconcileN(t, r, 1)
	repo := getRepo(t, c)
	if repo.Status.AccountID != b2fake.AccountID {
		t.Fatalf("status.accountID = %q, want %q", repo.Status.AccountID, b2fake.AccountID)
	}

	base := repo.DeepCopy()
	repo.Status.AccountID = ""
	if err := c.Status().Patch(context.Background(), repo, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)
	if got := getRepo(t, c).Status.AccountID; got != b2fake.AccountID {
		t.Errorf("an existing Repository did not record its account: %q", got)
	}

	pointAtAnotherAccount(t, r, c)
	if err := reconcileErr(r); err == nil {
		t.Fatal("reconciled against another account")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond.Reason != reasonAccountMismatch {
		t.Errorf("Ready = %+v, want %s", cond, reasonAccountMismatch)
	}
}

func TestDataLeftForAnotherRepositoryKeepsItsPassword(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	bucket := f.AddBucket(sharedBucket)
	r, c := newHarness(t, f, prefixRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	reconcileN(t, r, 2)
	f.PutFile(bucket.BucketID, myappPath+"/config", 1)
	if err := c.Create(context.Background(), prefixRepo(func(r *b2backupv1.Repository) {
		r.Name, r.UID = "nested", "uid-nested"
		r.Spec.Path = myappPath + "/nested"
	})); err != nil {
		t.Fatal(err)
	}
	nested := &b2backupv1.Repository{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "nested"}, nested); err != nil {
		t.Fatal(err)
	}
	base := nested.DeepCopy()
	nested.Status = b2backupv1.RepositoryStatus{
		BucketID: bucket.BucketID, BucketName: sharedBucket, Path: myappPath + "/nested",
	}
	if err := c.Status().Patch(context.Background(), nested, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}

	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)
	if !hasEvent(recordedEvents(t, r), "DataRetained") {
		t.Fatal("precondition: the data should have been retained")
	}
	if n := len(f.FileNames(bucket.BucketID)); n != 1 {
		t.Fatalf("%d files left, want 1", n)
	}
	secret := getSecret(t, c, credsSecretName)
	if owner := metav1.GetControllerOf(secret); owner != nil {
		t.Errorf("the Secret is still controlled by %s, so garbage collection takes the password of retained data",
			owner.Name)
	}
	if !slices.Contains(secret.Finalizers, SecretFinalizer) {
		t.Error("the only copy of the password for retained data lost its finalizer")
	}
}

func TestAnInitJobOfAnotherRepositoryIsNeverTakenForThisOne(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	foreign := &batchv1.Job{
		Name: resticName + initJobSuffix, Namespace: testNS,
		Labels: map[string]string{labelManagedBy: managedByValue},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: b2backupv1.GroupVersion.String(), Kind: "Repository", Name: resticName,
			UID: "uid-deleted-predecessor", Controller: ptr.To(true),
		}},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	r, c := newHarness(t, f, prefixRepo(nil), foreign)
	reconcileN(t, r, 3)
	if getRepo(t, c).Status.Initialized {
		t.Fatal("a path never initialized was recorded as initialized from another Repository's init Job")
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionInitialized); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Initialized = %+v, want False while waiting", cond)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreign), &job); err != nil {
		t.Errorf("the other Repository's init Job was touched: %v", err)
	}
}

func TestFinalizeReadsStatusPastAStaleCache(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	var stale atomic.Bool
	r, c := newHarnessWithInterceptor(t, f, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if repo, ok := obj.(*b2backupv1.Repository); ok && stale.Load() {
				repo.Status = b2backupv1.RepositoryStatus{}
			}
			return nil
		},
	}, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	reconcileN(t, r, 2)
	bucket := f.Bucket(ownBucket)
	f.PutFile(bucket.BucketID, "data/00/aa", 1)

	stale.Store(true)
	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)
	if f.Bucket(ownBucket) != nil {
		t.Error("a finalize pass reading a cache from before provisioning left the data behind")
	}
}

func TestAddingTheFinalizerKeepsAnotherControllersFinalizer(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	const other = "example.com/other"
	var snapshot atomic.Pointer[b2backupv1.Repository]
	r, c := newHarnessWithInterceptor(t, f, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if repo, ok := obj.(*b2backupv1.Repository); ok && snapshot.Load() != nil {
				snapshot.Load().DeepCopyInto(repo)
				return nil
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, prefixRepo(nil))
	snapshot.Store(getRepo(t, c))
	live := &b2backupv1.Repository{}
	if err := r.APIReader.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: resticName}, live); err != nil {
		t.Fatal(err)
	}
	live.Finalizers = append(live.Finalizers, other)
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	_ = reconcileErr(r)
	snapshot.Store(nil)
	reconcileN(t, r, 1)

	got := getRepo(t, c).Finalizers
	if !slices.Contains(got, other) {
		t.Errorf("finalizers = %v: a patch from a stale cache dropped another controller's finalizer", got)
	}
	if !slices.Contains(got, FinalizerName) {
		t.Errorf("finalizers = %v, want %s added", got, FinalizerName)
	}
}

func TestAPasswordWrittenBeforeStatusWasRecordedKeepsItsFinalizer(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	var failFinal atomic.Bool
	var patches atomic.Int32
	r, c := newHarnessWithInterceptor(t, f, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
			patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := obj.(*b2backupv1.Repository); ok && failFinal.Load() && patches.Add(1) > 1 {
				return apierrors.NewServiceUnavailable("status write lost")
			}
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}, bucketRepo(func(r *b2backupv1.Repository) {
		r.Spec.DeletionPolicy = b2backupv1.DeletionPolicyDelete
	}))
	failFinal.Store(true)
	_ = reconcileErr(r)
	failFinal.Store(false)
	repo := getRepo(t, c)
	if repo.Status.BucketID != "" || repo.Status.ApplicationKeyID == "" {
		t.Fatalf("precondition: only the key should be recorded, got %+v", repo.Status)
	}
	if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Fatal("precondition: the Secret should carry the finalizer")
	}

	deleteRepo(t, c)
	reconcileUntilGone(t, r, c)
	if !slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
		t.Error("the only copy of a password written for a provisioned key lost its finalizer")
	}
}

func TestAnUnlabelledInitJobOfAnotherIsReported(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	foreign := &batchv1.Job{Name: resticName + initJobSuffix, Namespace: testNS}
	r, c := newHarness(t, f, prefixRepo(nil), foreign)
	for range 2 {
		_ = reconcileErr(r)
	}
	if cond := condition(t, c, b2backupv1.RepositoryConditionInitialized); cond == nil || cond.Reason != reasonInitJobNotOwned {
		t.Errorf("Initialized = %+v, want %s naming the Job in the way", cond, reasonInitJobNotOwned)
	}
	if getRepo(t, c).Status.Initialized {
		t.Error("recorded as initialized from a Job it does not own")
	}
}
