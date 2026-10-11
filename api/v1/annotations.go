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
	// that overwrites its claims or database, or ones it shares, runs. Its value
	// names the restore. A second such restore is refused while it is set, and
	// the operator keeps the CronJob suspended and starts no triggered backup.
	AnnotationRestoreLock = "b2backup.mcswain.dev/restore-lock"

	// AnnotationRestorePartial lists, as a JSON array, what a restore may have
	// left half-written: pvc/<claim>:<path> and
	// db/<engine>/<secret>/<host>/<database> entries, recorded before its first
	// write. While it is set the operator keeps the CronJob suspended and starts
	// no triggered backup, here and on every ScheduledBackup that mounts a named
	// claim or dumps a named database, so a partial restore is never saved over
	// good snapshots. An entry in another form holds every ScheduledBackup in the
	// namespace. A later complete restore of an entry removes it; b2bk resume
	// removes them all.
	AnnotationRestorePartial = "b2backup.mcswain.dev/restore-partial"

	// AnnotationProtectedBy is set on a credentials Secret to the UID of the
	// Repository that put the protect-password finalizer on it. A Repository
	// never removes a finalizer another Repository set.
	AnnotationProtectedBy = "b2backup.mcswain.dev/protected-by"

	// AnnotationReleasedBy is set on a credentials Secret to the UID of the
	// deleted Repository that left it behind holding the only copy of the
	// password for its data. No other Repository takes such a Secret over
	// while it keeps the protect-password finalizer.
	AnnotationReleasedBy = "b2backup.mcswain.dev/released-by"

	// AnnotationReleasedRepository is set beside AnnotationReleasedBy to the
	// RESTIC_REPOSITORY the password belongs to.
	AnnotationReleasedRepository = "b2backup.mcswain.dev/released-repository"
)
