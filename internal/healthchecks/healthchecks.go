// Package healthchecks wires backup runs up to Healthchecks.io via runitor.
package healthchecks

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

const (
	// EnvAPIURL is the runitor variable holding the Healthchecks endpoint.
	EnvAPIURL = "HC_API_URL"
	// EnvSlug is the runitor variable holding the check slug.
	EnvSlug = "CHECK_SLUG"
	// EnvPingKey is the runitor variable holding the project ping key.
	EnvPingKey = "PING_KEY"
	// EnvUUID is the runitor variable holding a check UUID.
	EnvUUID = "CHECK_UUID"
)

// Config is the operator-wide Healthchecks configuration.
type Config struct {
	Enabled bool

	APIURL string

	AutoCreate bool
}

// Overrides are the per-ScheduledBackup settings that take precedence over Config.
type Overrides struct {
	Enabled    *bool
	Create     *bool
	APIURL     string
	Slug       string
	PingKeyRef *corev1.SecretKeySelector
	UUIDRef    *corev1.SecretKeySelector
}

// Reporter is a resolved Healthchecks configuration for one backup.
type Reporter struct {
	enabled    bool
	create     bool
	apiURL     string
	slug       string
	pingKeyRef *corev1.SecretKeySelector
	uuidRef    *corev1.SecretKeySelector
}

// Resolve merges Overrides onto Config to produce a Reporter.
func Resolve(cfg Config, o Overrides, defaultSlug string) Reporter {
	r := Reporter{
		enabled:    cfg.Enabled,
		create:     cfg.AutoCreate,
		apiURL:     cfg.APIURL,
		slug:       defaultSlug,
		pingKeyRef: o.PingKeyRef,
		uuidRef:    o.UUIDRef,
	}
	if o.Enabled != nil {
		r.enabled = *o.Enabled
	}
	if o.Create != nil {
		r.create = *o.Create
	}
	if o.APIURL != "" {
		r.apiURL = o.APIURL
	}
	if o.Slug != "" {
		r.slug = o.Slug
	}
	return r
}

// Enabled reports whether runs should be pinged.
func (r Reporter) Enabled() bool { return r.enabled && r.ConfigError() == nil }

// ConfigError reports why Healthchecks reporting is unusable for this backup.
// Returns nil when reporting is off or correctly configured.
func (r Reporter) ConfigError() error {
	if !r.enabled {
		return nil
	}
	if r.apiURL == "" {
		return fmt.Errorf("healthchecks is enabled but no API URL is configured")
	}
	if r.PingsByUUID() {
		return nil
	}
	if r.pingKeyRef == nil {
		return fmt.Errorf(
			"healthchecks is enabled but neither pingKeySecretRef nor uuidSecretRef is set")
	}
	if r.slug == "" {
		return fmt.Errorf("healthchecks is enabled but no slug could be determined")
	}
	return nil
}

// Slug returns the check slug to ping.
func (r Reporter) Slug() string { return r.slug }

// PingsByUUID reports whether the check is addressed by UUID rather than slug.
func (r Reporter) PingsByUUID() bool { return r.uuidRef != nil }

// Wrap prefixes cmd with runitor so the run is pinged. Returns cmd unchanged when disabled.
func (r Reporter) Wrap(cmd []string) []string {
	if !r.Enabled() {
		return cmd
	}
	out := []string{"runitor"}

	if r.create && !r.PingsByUUID() {
		out = append(out, "-create")
	}
	out = append(out, "--")
	return append(out, cmd...)
}

// Env returns the environment variables runitor needs.
func (r Reporter) Env() []corev1.EnvVar {
	if !r.Enabled() {
		return nil
	}

	env := []corev1.EnvVar{{Name: EnvAPIURL, Value: r.apiURL}}

	if r.PingsByUUID() {
		return append(env, corev1.EnvVar{
			Name:      EnvUUID,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: r.uuidRef},
		})
	}

	return append(env,
		corev1.EnvVar{Name: EnvSlug, Value: r.slug},
		corev1.EnvVar{
			Name:      EnvPingKey,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: r.pingKeyRef},
		},
	)
}
