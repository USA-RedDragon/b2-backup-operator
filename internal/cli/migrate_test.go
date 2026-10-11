package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

func writeMigrationDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "helmrelease.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func helmReleaseWith(schedule, persistence, script string) string {
	return `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  namespace: prod
spec:
  values:
    controllers:
      restic:
        cronjob:
          schedule: "` + schedule + `"
          timeZone: America/Chicago
        containers:
          restic:
            workingDir: /data
            command:
              - sh
              - -c
              - |
                set -eu
` + indentScript(script) + `
    persistence:
` + persistence
}

func indentScript(script string) string {
	var lines []string
	for line := range strings.SplitSeq(strings.TrimRight(script, "\n"), "\n") {
		lines = append(lines, "                "+line)
	}
	return strings.Join(lines, "\n")
}

const oneClaim = `      data:
        existingClaim: app-data
`

func migrate(t *testing.T, body string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runMigrate(&buf, writeMigrationDir(t, body), "ab12cd34")
	return buf.String(), err
}

func TestShorthandScheduleOnlyEmitsSchedulesTheOperatorAccepts(t *testing.T) {
	for n := 1; n <= 23; n++ {
		expr := fmt.Sprintf("0 */%d * * *", n)
		got := shorthandSchedule(expr)
		if _, err := backup.ResolveSchedule(got, "prod/restic"); err != nil {
			t.Errorf("shorthandSchedule(%q) = %q, which the operator rejects: %v", expr, got, err)
		}
	}
}

func TestShorthandScheduleKeepsUnevenHourSteps(t *testing.T) {
	shortened := []struct {
		expr string
		want string
	}{
		{"36 * * * *", "@hourly"},
		{"20 3 * * *", "@daily"},
		{"27 */6 * * *", "@every 6h"},
		{"27 */4 * * *", "@every 4h"},
	}
	for _, tt := range shortened {
		if got := shorthandSchedule(tt.expr); got != tt.want {
			t.Errorf("shorthandSchedule(%q) = %q, want %q", tt.expr, got, tt.want)
		}
	}

	unchanged := []string{
		"0 */5 * * *", "0 */7 * * *", "0 */13 * * *", "0 */0 * * *", "0 3 * * 0",
	}
	for _, expr := range unchanged {
		if got := shorthandSchedule(expr); got != expr {
			t.Errorf("shorthandSchedule(%q) = %q, want it left alone", expr, got)
		}
	}
}

func TestMigrateEmitsAScheduleValidateAccepts(t *testing.T) {
	out, err := migrate(t, helmReleaseWith("0 */5 * * *", oneClaim, "restic backup /data"))
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "schedule: 0 */5 * * *") {
		t.Errorf("expected the raw expression to be handed through:\n%s", out)
	}
	if strings.Contains(out, "@every 5h") {
		t.Errorf("emitted @every 5h, which does not divide into a day:\n%s", out)
	}
	if strings.Contains(out, "Handed back to the operator") {
		t.Errorf("nothing was rescheduled, so nothing should be reported as handed back:\n%s", out)
	}

	path := filepath.Join(t.TempDir(), "out.yaml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := readScheduledBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBackup(&bytes.Buffer{}, sb); err != nil {
		t.Errorf("migrate emitted a manifest validate rejects: %v", err)
	}
}

func TestMigrateRefusesTwoPersistentClaims(t *testing.T) {
	persistence := `      data:
        existingClaim: app-data
      uploads:
        existingClaim: app-uploads
`
	out, err := migrate(t, helmReleaseWith("36 * * * *", persistence, "restic backup /data"))
	if !errors.Is(err, ErrMultipleClaims) {
		t.Fatalf("expected ErrMultipleClaims, got %v\n%s", err, out)
	}
	for _, want := range []string{"data", "uploads"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q named in %q", want, err)
		}
	}
}

func TestMigrateRefusesTwoDatabaseSecrets(t *testing.T) {
	persistence := `      creds:
        type: secret
        name: db-creds
      other:
        type: secret
        name: other-creds
`
	script := "restic backup --tag=db --stdin-from-command -- dumpdb cnpg"
	out, err := migrate(t, helmReleaseWith("36 * * * *", persistence, script))
	if !errors.Is(err, ErrMultipleDBSecrets) {
		t.Fatalf("expected ErrMultipleDBSecrets, got %v\n%s", err, out)
	}
}

func TestMigrateKeepsASingleClaim(t *testing.T) {
	out, err := migrate(t, helmReleaseWith("36 * * * *", oneClaim, "restic backup /data"))
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "existingClaim: app-data") {
		t.Errorf("expected the claim in the output:\n%s", out)
	}
}

func TestParseBackupLineReadsSpaceSeparatedExcludes(t *testing.T) {
	const wantPath = "/data"
	bothExcludes := []string{"cache", "tmp"}

	tests := []struct {
		name     string
		line     string
		wantExcl []string
	}{
		{
			name:     "equals form",
			line:     "restic backup /data --exclude=cache --exclude=tmp",
			wantExcl: bothExcludes,
		},
		{
			name:     "space form",
			line:     "restic backup /data --exclude cache --exclude tmp",
			wantExcl: bothExcludes,
		},
		{
			name:     "space form before the path",
			line:     "restic backup --exclude cache /data",
			wantExcl: []string{"cache"},
		},
		{
			name:     "mixed forms",
			line:     "restic backup /data --exclude cache --exclude=tmp",
			wantExcl: bothExcludes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source, _, err := parseBackupLine(splitArgs(tt.line))
			if err != nil {
				t.Fatalf("parseBackupLine(%q) error = %v", tt.line, err)
			}
			if source.Path != wantPath {
				t.Errorf("path = %q, want %q", source.Path, wantPath)
			}
			if strings.Join(source.Exclude, ",") != strings.Join(tt.wantExcl, ",") {
				t.Errorf("exclude = %v, want %v", source.Exclude, tt.wantExcl)
			}
		})
	}
}

func TestParseBackupLineRefusesADanglingExclude(t *testing.T) {
	_, _, err := parseBackupLine(splitArgs("restic backup /data --exclude"))
	if !errors.Is(err, ErrExcludeCount) {
		t.Errorf("expected ErrExcludeCount, got %v", err)
	}
}

func TestMigrateMountsTheClaimTheScriptBacksUp(t *testing.T) {
	out, err := migrate(t, helmReleaseWith("36 * * * *", oneClaim, "restic backup ."))
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}

	path := filepath.Join(t.TempDir(), "out.yaml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := readScheduledBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Spec.Volume == nil {
		t.Fatal("no volume was emitted")
	}
	if sb.Spec.Volume.ExistingClaim != "app-data" || sb.Spec.Volume.MountPath != "/data" {
		t.Errorf("mounted %q at %q, want app-data at /data",
			sb.Spec.Volume.ExistingClaim, sb.Spec.Volume.MountPath)
	}
	if len(sb.Spec.Sources) != 1 || sb.Spec.Sources[0].Type != b2backupv1.SourceTypeFiles {
		t.Fatalf("unexpected sources %+v", sb.Spec.Sources)
	}
}
