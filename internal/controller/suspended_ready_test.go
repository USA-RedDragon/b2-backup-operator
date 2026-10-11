package controller_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const suspendBucket = "acme-suspend"

func TestReadySaysWhenTheScheduleDoesNotFire(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		suspend     bool
		annotations map[string]string
		wantReason  string
		wantSays    string
		wantNot     string
	}{
		{name: "running on schedule", wantReason: "Ready", wantSays: "Backing up on schedule"},
		{name: "suspended", suspend: true, wantReason: "Suspended", wantSays: "spec.suspend",
			wantNot: "Backing up"},
		{name: "held by a restore",
			annotations: map[string]string{b2backupv1.AnnotationRestoreLock: "pvc/app-data"},
			wantReason:  "RestoreHeld", wantSays: "pvc/app-data", wantNot: "Backing up"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sb := &b2backupv1.ScheduledBackup{
				Namespace: lsNS, Name: lsName, UID: lsSBUID, Annotations: tt.annotations,
				Spec: b2backupv1.ScheduledBackupSpec{
					RepositoryRef: corev1.LocalObjectReference{Name: lsRepo},
					Schedule:      "@daily",
					Suspend:       tt.suspend,
					Volumes:       []b2backupv1.VolumeSpec{{ExistingClaim: "app-data", MountPath: "/data"}},
					Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}},
				},
			}
			repo := &b2backupv1.Repository{
				Namespace: lsNS, Name: lsRepo,
				Status: b2backupv1.RepositoryStatus{Initialized: true, BucketID: "b1", BucketName: suspendBucket},
			}
			r, c := lastSuccessfulHarness(t, sb, repo)
			for range 2 {
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sb)}); err != nil {
					t.Fatal(err)
				}
			}
			var got b2backupv1.ScheduledBackup
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(sb), &got); err != nil {
				t.Fatal(err)
			}
			cond := apimeta.FindStatusCondition(got.Status.Conditions, b2backupv1.ScheduledBackupConditionReady)
			if cond == nil {
				t.Fatal("no Ready condition")
			}
			if cond.Status != metav1.ConditionTrue || cond.Reason != tt.wantReason {
				t.Errorf("Ready = %s/%s, want True/%s", cond.Status, cond.Reason, tt.wantReason)
			}
			if !strings.Contains(cond.Message, tt.wantSays) || (tt.wantNot != "" && strings.Contains(cond.Message, tt.wantNot)) {
				t.Errorf("Ready message %q, want it to say %q and not %q", cond.Message, tt.wantSays, tt.wantNot)
			}
		})
	}
}
