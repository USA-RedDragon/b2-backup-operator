package backup

import (
	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// RestoreHold says why a restore holds the backup's schedule, or "" when none
// does: one is running, or one failed and may have left a target half-written.
// A held backup's CronJob is suspended whatever spec.suspend says, and no
// triggered backup starts, so a partial restore is never saved.
func RestoreHold(sb *b2backupv1.ScheduledBackup) string {
	if holder := sb.Annotations[b2backupv1.AnnotationRestoreLock]; holder != "" {
		return "a restore is running (" + holder + ")"
	}
	if partial := sb.Annotations[b2backupv1.AnnotationRestorePartial]; partial != "" {
		return "a failed restore may have left " + partial + " half-written; check it, then run b2bk resume " + sb.Name
	}
	return ""
}
