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
	plain := runner.ForBackup(&b2backupv1.ScheduledBackup{})
	set := runner.ForBackup(&b2backupv1.ScheduledBackup{Spec: b2backupv1.ScheduledBackupSpec{
		Env: map[string]string{backup.EnvProgressFPS: "1"},
	}})
	tests := []struct {
		name        string
		subject     runner.Subject
		interactive bool
		want        []string
	}{
		{"terminal, operator default", plain, true, []string{"env", "-u", backup.EnvProgressFPS, "sh"}},
		{"terminal, spec.env sets it", set, true, []string{"sh"}},
		{"no terminal", plain, false, []string{"sh"}},
		{"bare Repository", runner.Subject{Repository: &b2backupv1.Repository{}}, true, []string{"sh"}},
	}
	for _, tt := range tests {
		if got := shellArgv(tt.subject, "sh", tt.interactive); !slices.Equal(got, tt.want) {
			t.Errorf("%s: shellArgv = %v, want %v", tt.name, got, tt.want)
		}
	}
}
