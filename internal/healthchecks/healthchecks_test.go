package healthchecks

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func testConfig() Config {
	return Config{
		Enabled:    true,
		APIURL:     "http://healthchecks.healthchecks:8000/ping",
		AutoCreate: true,
	}
}

func pingKeyRef() *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{
		Name: "healthchecks-ping-key",
		Key:  "PING_KEY",
	}
}

func envOf(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

func TestResolveDefaultsSlugToTheGivenFallback(t *testing.T) {
	r := Resolve(testConfig(), Overrides{PingKeyRef: pingKeyRef()}, "myapp-prod")
	if !r.Enabled() {
		t.Error("expected reporting to be enabled by operator config")
	}
	if r.Slug() != "myapp-prod" {
		t.Errorf("Slug() = %q, want the fallback", r.Slug())
	}
}

func TestOverridesWinOverConfig(t *testing.T) {
	r := Resolve(testConfig(), Overrides{
		Slug:       "custom-slug",
		APIURL:     "https://hc-ping.com",
		Create:     ptr.To(false),
		PingKeyRef: pingKeyRef(),
	}, "myapp-prod")

	if r.Slug() != "custom-slug" {
		t.Errorf("Slug() = %q", r.Slug())
	}
	env := r.Env()
	if got := envOf(env, EnvAPIURL); got == nil || got.Value != "https://hc-ping.com" {
		t.Errorf("%s = %v", EnvAPIURL, got)
	}
	if slices.Contains(r.Wrap([]string{"true"}), "-create") {
		t.Error("create=false must not pass -create")
	}
}

func TestSlugMode(t *testing.T) {
	r := Resolve(testConfig(), Overrides{PingKeyRef: pingKeyRef()}, "myapp-prod")

	cmd := []string{"sh", "-c", "restic backup"}
	want := append([]string{"runitor", "-create", "--"}, cmd...)
	if got := r.Wrap(cmd); !slices.Equal(got, want) {
		t.Errorf("Wrap() = %v, want %v", got, want)
	}

	env := r.Env()
	if got := envOf(env, EnvSlug); got == nil || got.Value != "myapp-prod" {
		t.Errorf("%s = %v", EnvSlug, got)
	}
	if got := envOf(env, EnvPingKey); got == nil || got.ValueFrom == nil ||
		got.ValueFrom.SecretKeyRef.Name != "healthchecks-ping-key" {
		t.Errorf("%s must come from the per-resource secret ref", EnvPingKey)
	}
	if envOf(env, EnvUUID) != nil {
		t.Errorf("slug mode must not set %s", EnvUUID)
	}
}

func TestUUIDAdoptionMode(t *testing.T) {
	r := Resolve(testConfig(), Overrides{
		UUIDRef: &corev1.SecretKeySelector{
			Name: "restic-envs",
			Key:  "CHECK_UUID",
		},
	}, "myapp-prod")

	env := r.Env()
	if envOf(env, EnvUUID) == nil {
		t.Errorf("expected %s on the adoption path", EnvUUID)
	}
	if envOf(env, EnvSlug) != nil {
		t.Errorf("uuid mode must not also set %s", EnvSlug)
	}

	if slices.Contains(r.Wrap([]string{"true"}), "-create") {
		t.Error("uuid mode must not pass -create")
	}
}

func TestDisabledLeavesCommandAndEnvAlone(t *testing.T) {
	r := Resolve(testConfig(), Overrides{Enabled: ptr.To(false)}, "myapp-prod")

	cmd := []string{"sh", "-c", "restic check"}
	if got := r.Wrap(cmd); !slices.Equal(got, cmd) {
		t.Errorf("Wrap() = %v, want the command unchanged", got)
	}
	if env := r.Env(); env != nil {
		t.Errorf("Env() = %v, want nil when disabled", env)
	}
	if err := r.ConfigError(); err != nil {
		t.Errorf("ConfigError() = %v, want nil when disabled", err)
	}
}

func TestMisconfiguredReportersDegrade(t *testing.T) {
	for name, r := range map[string]Reporter{
		"no key":     Resolve(testConfig(), Overrides{}, "myapp-prod"),
		"no api url": Resolve(Config{Enabled: true}, Overrides{PingKeyRef: pingKeyRef()}, "myapp-prod"),
		"no slug":    Resolve(testConfig(), Overrides{PingKeyRef: pingKeyRef()}, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if err := r.ConfigError(); err == nil {
				t.Fatal("expected ConfigError() to report the misconfiguration")
			}
			if r.Enabled() {
				t.Error("a misconfigured reporter must not report itself as enabled")
			}
			cmd := []string{"sh", "-c", "restic backup"}
			if got := r.Wrap(cmd); !slices.Equal(got, cmd) {
				t.Errorf("Wrap() = %v, want the command unchanged", got)
			}
			if env := r.Env(); env != nil {
				t.Errorf("Env() = %v, want nil", env)
			}
		})
	}
}
