package backup

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// LastSuccess is when the backup last succeeded: status.lastSuccessfulTime or
// the newest succeeded run in its history, whichever is later, since an older
// operator took lastSuccessfulTime from the CronJob alone and left out
// triggered runs.
func LastSuccess(sb *b2backupv1.ScheduledBackup) *metav1.Time {
	latest := sb.Status.LastSuccessfulTime
	for i := range sb.Status.History {
		run := &sb.Status.History[i]
		if run.Result != b2backupv1.BackupRunSucceeded {
			continue
		}
		at := run.CompletionTime
		if at == nil {
			at = run.StartTime
		}
		if at != nil && (latest == nil || at.After(latest.Time)) {
			latest = at
		}
	}
	return latest
}
