package v1

const (
	// AnnotationTriggerAt requests a one-off backup. Its value is an RFC3339 timestamp.
	AnnotationTriggerAt = "b2backup.mcswain.dev/trigger-at"

	// AnnotationForceFinalize releases a Repository whose B2 data or application
	// key cannot be deleted, leaving them behind. Its value must be "true".
	AnnotationForceFinalize = "b2backup.mcswain.dev/force-finalize"

	// AnnotationReinitAt asks the operator to delete the Repository's init Job and
	// run restic init again. Its value is an RFC3339 timestamp; a value no later
	// than status.lastReinitTime is ignored.
	AnnotationReinitAt = "b2backup.mcswain.dev/reinit-at"

	// AnnotationRestoreLock is set by b2bk on a ScheduledBackup while a restore
	// that overwrites its claims or database runs. Its value names the restore.
	// A second such restore is refused while it is set.
	AnnotationRestoreLock = "b2backup.mcswain.dev/restore-lock"

	// AnnotationRestoreResume is "true" when b2bk suspended the ScheduledBackup
	// for a restore. The restore that next succeeds resumes it; b2bk suspend and
	// b2bk resume clear it.
	AnnotationRestoreResume = "b2backup.mcswain.dev/restore-resume"
)
