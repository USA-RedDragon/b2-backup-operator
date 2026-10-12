package cli

import "testing"

func TestImageTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		image string
		want  string
	}{
		{"jacob.pub/ghcr.io/usa-reddragon/b2-backup-operator:v0.1.6@sha256:015e36e8e307979a80eca82358758433381da5fdb13c1dc08281edc4e147ebaa", "v0.1.6"},
		{"ghcr.io/usa-reddragon/b2-backup-operator:v0.1.6", "v0.1.6"},
		{"ghcr.io/usa-reddragon/b2-backup-operator@sha256:015e36e8e307979a80eca82358758433381da5fdb13c1dc08281edc4e147ebaa", ""},
		{"registry.local:5000/b2-backup-operator", ""},
		{"registry.local:5000/b2-backup-operator:dev", "dev"},
		{"b2-backup-operator", ""},
	}
	for _, tt := range tests {
		if got := imageTag(tt.image); got != tt.want {
			t.Errorf("imageTag(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}
