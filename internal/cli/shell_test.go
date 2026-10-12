package cli

import (
	"slices"
	"testing"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

func TestShellDropsTheDefaultProgressFPSOnATerminal(t *testing.T) {
	t.Parallel()
	const env = "env"
	plain := runner.ForBackup(&b2backupv1.ScheduledBackup{})
	set := runner.ForBackup(&b2backupv1.ScheduledBackup{Spec: b2backupv1.ScheduledBackupSpec{
		Env: map[string]string{backup.EnvProgressFPS: "1"},
	}})
	tests := []struct {
		name        string
		subject     runner.Subject
		interactive bool
		noColor     string
		want        []string
	}{
		{"terminal, operator default", plain, true, "", []string{env, "-u", backup.EnvProgressFPS, "sh"}},
		{"terminal, spec.env sets it", set, true, "", []string{"sh"}},
		{"no terminal", plain, false, "", []string{"sh"}},
		{"bare Repository", runner.Subject{Repository: &b2backupv1.Repository{}}, true, "", []string{"sh"}},
		{"terminal, NO_COLOR set locally", plain, true, "1", []string{env, "-u", backup.EnvProgressFPS, "NO_COLOR=1", "sh"}},
		{"spec.env sets fps, NO_COLOR set locally", set, true, "yes", []string{env, "NO_COLOR=yes", "sh"}},
		{"no terminal, NO_COLOR set locally", plain, false, "1", []string{env, "NO_COLOR=1", "sh"}},
	}
	for _, tt := range tests {
		if got := shellArgv(tt.subject, "sh", tt.interactive, tt.noColor); !slices.Equal(got, tt.want) {
			t.Errorf("%s: shellArgv = %v, want %v", tt.name, got, tt.want)
		}
	}
}
