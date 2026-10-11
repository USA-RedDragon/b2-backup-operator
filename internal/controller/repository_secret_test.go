package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
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
		{"a generated password under Delete", b2backupv1.DeletionPolicyDelete, nil, false, false},
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
		if slices.Contains(getSecret(t, c, credsSecretName).Finalizers, SecretFinalizer) {
			t.Error("a password preserved in its seed kept the Secret finalizer")
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
