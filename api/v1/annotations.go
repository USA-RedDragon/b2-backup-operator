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
)
