package cli_test

import (
	"testing"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
)

func TestKnownImagesThroughARegistryMirror(t *testing.T) {
	t.Parallel()
	const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		image    string
		operator bool
		upstream bool
	}{
		{image: "ghcr.io/usa-reddragon/restic:0.19.1", operator: true},
		{image: "ghcr.io/usa-reddragon/restic" + digest, operator: true},
		{image: "ghcr.io/usa-reddragon/restic", operator: true},
		{image: "jacob.pub/ghcr.io/usa-reddragon/restic:0.19.1", operator: true},
		{image: "jacob.pub/ghcr.io/usa-reddragon/restic:0.19.1" + digest, operator: true},
		{image: "mirror.example:5000/ghcr.io/usa-reddragon/restic" + digest, operator: true},
		{image: "jacob.pub/ghcr.io/gabe565/restic:0.18.1", upstream: true},
		{image: "ghcr.io/gabe565/restic:0.18.1", upstream: true},
		{image: "ghcr.io/usa-reddragon/restic-fork:1"},
		{image: "ghcr.io/other/usa-reddragon/restic-x:1"},
		{image: "docker.io/restic/restic:0.19.1"},
	}
	for _, tc := range cases {
		t.Run(tc.image, func(t *testing.T) {
			t.Parallel()
			if got := cli.IsOperatorImage(tc.image); got != tc.operator {
				t.Errorf("IsOperatorImage = %t, want %t", got, tc.operator)
			}
			if got := cli.IsUpstreamImage(tc.image); got != tc.upstream {
				t.Errorf("IsUpstreamImage = %t, want %t", got, tc.upstream)
			}
		})
	}
}
