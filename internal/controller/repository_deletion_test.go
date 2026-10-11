package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
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
