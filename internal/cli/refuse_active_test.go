package cli_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	operatorName  = "b2-backup-operator"
	purposeLabel  = "b2backup.mcswain.dev/b2bk-purpose"
	backupJobName = pvcBackup + "-29312345"
	restoreJob    = pvcBackup + "-b2bk-restore-q1w2e"
	kindBackup    = "ScheduledBackup"
)

func TestOverwritingRestoreNamesWhatIsStillRunning(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		running client.Object
		wantErr error
		want    []string
		notWant []string
	}{
		{name: "a backup Job", running: &batchv1.Job{
			Namespace: pvcNS, Name: backupJobName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: b2backupv1.GroupVersion.String(), Kind: kindBackup, Name: pvcBackup,
				UID: pvcSBUID, Controller: ptr.To(true),
			}},
		}, wantErr: cli.ErrBackupRunning, want: []string{"job/" + backupJobName + " (a backup)", "b2bk wait"}},
		{name: "a terminating b2bk restore pod", running: &corev1.Pod{
			Namespace: pvcNS, Name: restoreJob + "-abcde",
			DeletionTimestamp: ptr.To(metav1.Now()), Finalizers: []string{"test/hold"},
			Labels: map[string]string{
				managedByKey: runner.ManagedByValue, runner.LabelScheduledBackup: pvcBackup,
				purposeLabel: "restore", batchv1.JobNameLabel: restoreJob,
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}, wantErr: cli.ErrRestoreRunning,
			want:    []string{"a pod of a b2bk files restore, Terminating", "--force --grace-period=0"},
			notWant: []string{"a backup is running", "b2bk cancel"}},
		{name: "a terminating backup pod", running: &corev1.Pod{
			Namespace: pvcNS, Name: backupJobName + "-abcde",
			DeletionTimestamp: ptr.To(metav1.Now()), Finalizers: []string{"test/hold"},
			Labels: map[string]string{
				managedByKey: operatorName, "app.kubernetes.io/name": pvcBackup,
				batchv1.JobNameLabel: backupJobName,
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}, wantErr: cli.ErrBackupRunning, want: []string{"a pod of a backup, Terminating"}},
		{name: "a b2bk database restore Job", running: &batchv1.Job{
			Namespace: pvcNS, Name: pvcBackup + "-b2bk-restoredb-z9x8c",
			Labels: map[string]string{managedByKey: runner.ManagedByValue, purposeLabel: "restoredb"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: b2backupv1.GroupVersion.String(), Kind: kindBackup, Name: pvcBackup, UID: pvcSBUID,
			}},
		}, wantErr: cli.ErrRestoreRunning, want: []string{"a b2bk database restore", "kubectl get job"},
			notWant: []string{"a backup is running", "b2bk cancel"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := pvcScheduledBackup()
			c := pvcClient(t, sb, tc.running)
			err := cli.RefuseActive(context.Background(), c, sb, "")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err %q does not say %q", err, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(err.Error(), w) {
					t.Errorf("err %q says %q", err, w)
				}
			}
		})
	}
}
