package v1

const (
	// AnnotationTriggerAt requests a one-off backup. Its value is an RFC3339 timestamp.
	AnnotationTriggerAt = "b2backup.mcswain.dev/trigger-at"

	// AnnotationForceFinalize releases a Repository whose BorgBase repository
	// cannot be deleted, leaving that repository behind. Its value must be "true".
	AnnotationForceFinalize = "b2backup.mcswain.dev/force-finalize"
)
