package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
	"github.com/USA-RedDragon/b2-backup-operator/internal/b2/b2fake"
)

func eventWith(recorded []string, reason, text string) bool {
	return slices.ContainsFunc(recorded, func(e string) bool {
		return strings.Contains(e, " "+reason+" ") && strings.Contains(e, text)
	})
}

func TestRetainKeepsTheDataButNotTheKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		seeded     bool
		wantSecret bool
		wantEvent  string
	}{
		{name: "a seeded password deletes the Secret", seeded: true,
			wantEvent: "deleted credentials Secret " + credsSecretName + ", since Secret " + seedSecret},
		{name: "a generated password keeps the Secret with the password alone", wantSecret: true,
			wantEvent: "kept credentials Secret " + credsSecretName + " with only RESTIC_PASSWORD and RESTIC_REPOSITORY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			objs := []client.Object{bucketRepo(func(r *b2backupv1.Repository) {
				if tt.seeded {
					usePasswordSeed(r)
				}
			})}
			if tt.seeded {
				objs = append(objs, seedSecretFixture())
			}
			r, c := newHarness(t, f, objs...)
			repo := provisionForDeletion(t, r, c)
			bucket := f.Bucket(ownBucket)
			f.PutFile(bucket.BucketID, "config", 1)
			password := string(getSecret(t, c, credsSecretName).Data[KeyResticPassword])

			reconcileUntilGone(t, r, c)
			if f.Bucket(ownBucket) == nil || len(f.FileNames(bucket.BucketID)) != 1 {
				t.Error("Retain deleted data")
			}
			if keys := ownKeys(f); len(keys) != 0 {
				t.Errorf("Retain kept application keys %+v", keys)
			}
			recorded := recordedEvents(t, r)
			if !eventWith(recorded, "RepositoryRetained", "deleted application key "+repo.Status.ApplicationKeyID) ||
				!eventWith(recorded, "RepositoryRetained", tt.wantEvent) {
				t.Errorf("no RepositoryRetained event naming the key and saying %q in %q", tt.wantEvent, recorded)
			}

			err := getSecretErr(c)
			if !tt.wantSecret {
				if !apierrors.IsNotFound(err) {
					t.Errorf("the credentials Secret of a seeded password outlived its key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the only copy of a generated password was deleted: %v", err)
			}
			secret := getSecret(t, c, credsSecretName)
			if got := string(secret.Data[KeyResticPassword]); got != password {
				t.Errorf("password = %q, want %q", got, password)
			}
			if len(secret.Data[KeyResticRepository]) == 0 {
				t.Error("RESTIC_REPOSITORY was removed")
			}
			for _, key := range []string{KeyAccessKeyID, KeySecretAccessKey, KeyDefaultRegion} {
				if _, ok := secret.Data[key]; ok {
					t.Errorf("%s of a deleted key is still in the Secret", key)
				}
			}
			if !slices.Contains(secret.Finalizers, SecretFinalizer) {
				t.Error("the only copy of a generated password lost its finalizer")
			}
		})
	}
}

func TestRetainKeepsTheFinalizerUntilItsKeyIsDeleted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		breakIt func(r *RepositoryReconciler, f *b2fake.Server)
	}{
		{name: "the B2 credentials are gone", breakIt: func(r *RepositoryReconciler, _ *b2fake.Server) {
			r.DefaultCredentialsSecret.Name = missingSecret
		}},
		{name: "B2 refuses the deletion", breakIt: func(_ *RepositoryReconciler, f *b2fake.Server) {
			f.Fail("b2_delete_key", b2fake.Failure{Status: 503, Code: b2.CodeServiceUnavailable, Times: 1000})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			r, c := newHarness(t, f, bucketRepo(nil))
			provisionForDeletion(t, r, c)
			tt.breakIt(r, f)
			for range 3 {
				if err := reconcileErr(r); err == nil {
					t.Fatal("finalize succeeded without deleting the key")
				}
			}
			if cond := condition(t, c, b2backupv1.RepositoryConditionReady); cond == nil ||
				cond.Reason != "KeyDeletionFailed" || !strings.Contains(cond.Message, b2backupv1.AnnotationForceFinalize) {
				t.Errorf("Ready = %+v, want KeyDeletionFailed naming %s", cond, b2backupv1.AnnotationForceFinalize)
			}
			if len(ownKeys(f)) != 1 {
				t.Fatal("precondition: the key is still in B2")
			}

			annotateRepo(t, c, b2backupv1.AnnotationForceFinalize, "true")
			reconcileUntilGone(t, r, c)
			if len(ownKeys(f)) != 1 {
				t.Error("force-finalize was taken to have deleted the key")
			}
			if !hasEvent(recordedEvents(t, r), "KeyLeftBehind") {
				t.Error("no KeyLeftBehind event")
			}
			secret := getSecret(t, c, credsSecretName)
			if len(secret.Data[KeySecretAccessKey]) == 0 || len(secret.Data[KeyResticPassword]) == 0 {
				t.Error("force-finalize removed the key that is left in B2 from the Secret")
			}
			if !slices.Contains(secret.Finalizers, SecretFinalizer) {
				t.Error("the only copy of a generated password lost its finalizer")
			}
		})
	}

	t.Run("a deletion that recovers releases the Repository", func(t *testing.T) {
		t.Parallel()
		f := newFakeB2(t)
		r, c := newHarness(t, f, bucketRepo(nil))
		provisionForDeletion(t, r, c)
		f.Fail("b2_delete_key", b2fake.Failure{Status: 400, Code: b2.CodeBadRequest, Times: 1})
		if err := reconcileErr(r); err == nil {
			t.Fatal("finalize succeeded without deleting the key")
		}
		reconcileUntilGone(t, r, c)
		if keys := ownKeys(f); len(keys) != 0 {
			t.Errorf("keys left: %+v", keys)
		}
	})
}

func TestRetainDeletesNoOtherKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		keyName string
	}{
		{name: "another Repository of the same name", keyName: "b2bk-" + testNS + "-" + resticName + "-ffffffffffff"},
		{name: "a key someone made by hand", keyName: "hand-made"},
		{name: "another Repository in the namespace", keyName: "b2bk-" + testNS + "-other-0b1c2d3e0000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			r, c := newHarness(t, f, bucketRepo(nil))
			provisionForDeletion(t, r, c)
			other := f.AddKey(tt.keyName, f.Bucket(ownBucket).BucketID, "")
			repo := getRepo(t, c)
			base := repo.DeepCopy()
			repo.Status.ApplicationKeyID = other
			if err := c.Status().Patch(context.Background(), repo, client.MergeFrom(base)); err != nil {
				t.Fatal(err)
			}
			reconcileUntilGone(t, r, c)
			if !slices.ContainsFunc(f.Keys(), func(k b2.Key) bool { return k.ApplicationKeyID == other }) {
				t.Errorf("deleted key %s named %s, which this Repository never created", other, tt.keyName)
			}
			if slices.ContainsFunc(f.Keys(), func(k b2.Key) bool { return k.KeyName == keyName(bucketRepo(nil)) }) {
				t.Error("the Repository's own key survived")
			}
		})
	}
}
