package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
)

func TestStatusAndGetShowTheLastManualSuccess(t *testing.T) {
	t.Parallel()
	ago := func(d time.Duration) *metav1.Time { return ptr.To(metav1.NewTime(time.Now().Add(-d))) }
	cases := []struct {
		name     string
		recorded *metav1.Time
		history  []b2backupv1.BackupRun
		want     string
	}{
		{name: "a manual run an older operator left out", history: []b2backupv1.BackupRun{
			{JobName: "m1", Trigger: b2backupv1.BackupTriggerManual, Result: b2backupv1.BackupRunSucceeded,
				StartTime: ago(2 * time.Hour), CompletionTime: ago(2*time.Hour - 5*time.Minute)},
		}, want: "115m ago"},
		{name: "a failed run is no success", history: []b2backupv1.BackupRun{
			{JobName: "m1", Trigger: b2backupv1.BackupTriggerManual, Result: b2backupv1.BackupRunFailed,
				StartTime: ago(2 * time.Hour), CompletionTime: ago(2*time.Hour - 5*time.Minute)},
		}, want: "<none>"},
		{name: "the recorded time when it is newer", recorded: ago(30 * time.Minute), history: []b2backupv1.BackupRun{
			{JobName: "m1", Trigger: b2backupv1.BackupTriggerManual, Result: b2backupv1.BackupRunSucceeded,
				StartTime: ago(3 * time.Hour), CompletionTime: ago(3 * time.Hour)},
		}, want: "30m ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := pvcScheduledBackup()
			sb.Spec.Volumes, sb.Spec.Sources = nil, []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}}
			sb.Status.LastSuccessfulTime = tc.recorded
			sb.Status.History = tc.history
			repo := &b2backupv1.Repository{Namespace: pvcNS, Name: pvcRepo}
			c := pvcClient(t, sb, repo, &corev1.Secret{Namespace: pvcNS, Name: repo.SecretName()})

			var status, get bytes.Buffer
			if err := cli.Status(context.Background(), c, &status, pvcNS, "sb/"+pvcBackup, 5); err != nil {
				t.Fatal(err)
			}
			if err := cli.PrintScheduledBackups(&get, []b2backupv1.ScheduledBackup{*sb}, false, ""); err != nil {
				t.Fatal(err)
			}
			line := ""
			for l := range strings.Lines(status.String()) {
				if strings.Contains(l, "Last successful") {
					line = l
				}
			}
			if !strings.Contains(line, tc.want) {
				t.Errorf("status says %q, want %q", strings.TrimSpace(line), tc.want)
			}
			if !strings.Contains(get.String(), tc.want) {
				t.Errorf("get does not show %q:\n%s", tc.want, get.String())
			}
		})
	}
}
