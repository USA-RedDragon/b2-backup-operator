package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/USA-RedDragon/b2-backup-operator/internal/b2"
)

func runningJob(name, managedBy string, created time.Time, secret string) *batchv1.Job {
	return &batchv1.Job{
		Name: name, Namespace: testNS, CreationTimestamp: metav1.NewTime(created),
		Labels: map[string]string{labelManagedBy: managedBy},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: resticName, EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{Name: secret}}},
			}},
		}}},
		Status: batchv1.JobStatus{Active: 1, StartTime: ptr.To(metav1.NewTime(created))},
	}
}

const b2bkManagedBy = "b2bk"

func TestSupersededKeysWaitForJobsThatMayHoldThem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		jobs     func(rotated time.Time) []client.Object
		advance  time.Duration
		kept     bool
		deadline bool
	}{
		{"a backup started before the rotation holds it", func(rot time.Time) []client.Object {
			return []client.Object{runningJob("web-backup-1", managedByValue, rot.Add(-time.Minute), credsSecretName)}
		}, time.Hour, true, false},
		{"a b2bk Job started before the rotation holds it", func(rot time.Time) []client.Object {
			return []client.Object{runningJob("web-b2bk-restore-abcde", b2bkManagedBy, rot.Add(-time.Minute), credsSecretName)}
		}, time.Hour, true, false},
		{"a Job of no one's started before the rotation holds it", func(rot time.Time) []client.Object {
			return []client.Object{runningJob("hand-made", "", rot.Add(-time.Minute), credsSecretName)}
		}, time.Hour, true, false},
		{"a Job started after the rotation does not", func(rot time.Time) []client.Object {
			return []client.Object{runningJob("web-backup-2", managedByValue, rot.Add(time.Minute), credsSecretName)}
		}, time.Hour, false, false},
		{"a Job reading another Secret does not", func(rot time.Time) []client.Object {
			return []client.Object{runningJob("other-backup", managedByValue, rot.Add(-time.Minute), "other-credentials")}
		}, time.Hour, false, false},
		{"the 24 hour cap deletes it anyway", func(rot time.Time) []client.Object {
			return []client.Object{runningJob("web-backup-1", managedByValue, rot.Add(-time.Minute), credsSecretName)}
		}, 25 * time.Hour, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			f.AddBucket(sharedBucket)
			now := time.Now().Truncate(time.Second)
			r, c := newHarness(t, f, prefixRepo(usePasswordSeed), seedSecretFixture())
			r.Now = func() time.Time { return now }
			reconcileN(t, r, 1)
			succeedInitJob(t, c)
			oldKey := getRepo(t, c).Status.ApplicationKeyID

			loseSecret(t, c)
			reconcileN(t, r, 1)
			repo := getRepo(t, c)
			if repo.Status.KeyRotationTime == nil || !repo.Status.KeyRotationTime.Time.Equal(now) {
				t.Fatalf("keyRotationTime = %v, want %v", repo.Status.KeyRotationTime, now)
			}
			for _, obj := range tt.jobs(now) {
				if err := c.Create(context.Background(), obj); err != nil {
					t.Fatal(err)
				}
			}

			now = now.Add(tt.advance)
			res := reconcileOnce(t, r)
			kept := slices.ContainsFunc(ownKeys(f), func(k b2.Key) bool { return k.ApplicationKeyID == oldKey })
			if kept != tt.kept {
				t.Errorf("old key kept = %t, want %t", kept, tt.kept)
			}
			if tt.kept && (res.RequeueAfter == 0 || res.RequeueAfter > staleKeyRecheck) {
				t.Errorf("requeue after %v, want at most %v while waiting", res.RequeueAfter, staleKeyRecheck)
			}
			if got := hasEvent(recordedEvents(t, r), "StaleKeyDeadline"); got != tt.deadline {
				t.Errorf("StaleKeyDeadline event = %t, want %t", got, tt.deadline)
			}
		})
	}
}

func TestAStalePassNeverDeletesTheNewKey(t *testing.T) {
	t.Parallel()
	f := newFakeB2(t)
	f.AddBucket(sharedBucket)
	r, c := newHarness(t, f, prefixRepo(usePasswordSeed), seedSecretFixture())
	reconcileN(t, r, 1)
	succeedInitJob(t, c)
	stale := getRepo(t, c).DeepCopy()

	loseSecret(t, c)
	reconcileN(t, r, 1)
	newKey := getRepo(t, c).Status.ApplicationKeyID

	keys := f.Keys()
	if _, err := r.deleteStaleKeys(context.Background(), stale, r.NewAPI(masterKeyID, masterKey), keys); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(ownKeys(f), func(k b2.Key) bool { return k.ApplicationKeyID == newKey }) {
		t.Error("a pass reading a Repository from before the rotation deleted the key now in use")
	}
}
