package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

func writeHelmRelease(t *testing.T, schedule string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helmrelease.yaml")
	body := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
spec:
  values:
    controllers:
      restic:
        cronjob:
          schedule: "` + schedule + `"
          timeZone: America/Chicago
        containers:
          restic:
            command:
              - sh
              - -c
              - |
                exec > >(ts '%H:%M:%S') 2>&1
                set -eu
                restic backup --tag=db --stdin-from-command -- dumpdb cnpg
                restic cache --cleanup
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeGenerated(t *testing.T, schedule string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "generated.yaml")
	body := `apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: web-files
  namespace: prod
spec:
  repositoryRef:
    name: store
  schedule: "` + schedule + `"
  timeZone: America/Chicago
  sources:
    - type: cnpg
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func parity(t *testing.T, generated, original string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runParity(&buf, writeGenerated(t, generated), writeHelmRelease(t, original))
	return buf.String(), err
}

func TestParityAcceptsAReJitteredSchedule(t *testing.T) {
	out, err := parity(t, "@hourly", "36 * * * *")
	if err != nil {
		t.Fatalf("a re-jittered schedule should pass: %v\n%s", err, out)
	}
	for _, want := range []string{"RESCHEDULED", "EQUIVALENT"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestParityCatchesACadenceChange(t *testing.T) {
	out, err := parity(t, "@daily", "36 * * * *")
	if !errors.Is(err, ErrParityDiffers) {
		t.Fatalf("expected ErrParityDiffers, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "CADENCE DIFFERS") {
		t.Errorf("expected CADENCE DIFFERS in output:\n%s", out)
	}
}

func TestParityIdenticalWhenNothingMoved(t *testing.T) {
	out, err := parity(t, "36 * * * *", "36 * * * *")
	if err != nil {
		t.Fatalf("unexpected difference: %v\n%s", err, out)
	}
	if !strings.Contains(out, "IDENTICAL") {
		t.Errorf("expected IDENTICAL in output:\n%s", out)
	}
	if strings.Contains(out, "RESCHEDULED") {
		t.Errorf("nothing moved, so nothing should be reported as rescheduled:\n%s", out)
	}
}

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const filesBackup = `apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: web
  namespace: prod
spec:
  repositoryRef:
    name: store
  schedule: "@daily"
  sources:
    - type: files
      path: /data
`

func TestRenderCronJobUsesTheOperatorsDefaults(t *testing.T) {
	sb, err := readScheduledBackup(writeManifest(t, filesBackup))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := renderBackup(&buf, sb, true); err != nil {
		t.Fatalf("renderBackup: %v", err)
	}
	out := buf.String()

	defaults := backup.DefaultConfig()
	if !strings.Contains(out, defaults.Image) {
		t.Errorf("expected the operator's default image %q:\n%s", defaults.Image, out)
	}
	if strings.Contains(out, "restic:latest") {
		t.Errorf("rendered a :latest image the operator never uses:\n%s", out)
	}
}

func TestRenderCronJobMatchesTheOperator(t *testing.T) {
	sb, err := readScheduledBackup(writeManifest(t, filesBackup))
	if err != nil {
		t.Fatal(err)
	}
	repo := &b2backupv1.Repository{Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name}
	want, err := backup.BuildCronJob(sb, repo, backup.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	wantYAML, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := renderBackup(&buf, sb, true); err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(wantYAML) {
		t.Errorf("render --cronjob does not match what the operator would build:\n%s", buf.String())
	}
}

func TestRenderPrintsTheScript(t *testing.T) {
	sb, err := readScheduledBackup(writeManifest(t, filesBackup))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := renderBackup(&buf, sb, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "restic backup") {
		t.Errorf("expected a restic backup line:\n%s", buf.String())
	}
}

func TestValidateAcceptsAManifestTheOperatorRuns(t *testing.T) {
	sb, err := readScheduledBackup(writeManifest(t, filesBackup))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := validateBackup(&buf, sb); err != nil {
		t.Fatalf("validateBackup: %v", err)
	}
	for _, want := range []string{"is valid", "schedule", "sources   1"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("expected %q in:\n%s", want, buf.String())
		}
	}
}

func TestValidateRejectsWhatTheOperatorRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "unresolvable schedule",
			body: strings.Replace(filesBackup, `schedule: "@daily"`, `schedule: "@every 5h"`, 1),
			want: "spec.schedule",
		},
		{
			name: "unknown source type",
			body: strings.Replace(filesBackup, "type: files", "type: sqlite", 1),
			want: "rendering the script",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb, err := readScheduledBackup(writeManifest(t, tt.body))
			if err != nil {
				t.Fatal(err)
			}
			err = validateBackup(&bytes.Buffer{}, sb)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestReadScheduledBackupPicksTheRightDocument(t *testing.T) {
	body := `apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: store
  namespace: prod
spec:
  mode: Prefix
  bucket: acme-backups
---
apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: web
  namespace: prod
spec:
  repositoryRef:
    name: store
  schedule: "@daily"
  sources:
    - type: files
      path: /data
`
	sb, err := readScheduledBackup(writeManifest(t, body))
	if err != nil {
		t.Fatalf("readScheduledBackup: %v", err)
	}
	if sb.Name != "web" {
		t.Errorf("read %q, want the ScheduledBackup document", sb.Name)
	}
}

func TestReadScheduledBackupSurvivesASeparatorInTheScript(t *testing.T) {
	body := `apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: web
  namespace: prod
spec:
  repositoryRef:
    name: store
  schedule: "@daily"
  script: |
    echo kind: ScheduledBackup
    echo ---
    restic backup /data
  sources:
    - type: files
      path: /data
`
	sb, err := readScheduledBackup(writeManifest(t, body))
	if err != nil {
		t.Fatalf("readScheduledBackup: %v", err)
	}
	if sb.Name != "web" || len(sb.Spec.Sources) != 1 {
		t.Errorf("a --- inside spec.script truncated the document: %+v", sb.Spec)
	}
}

func TestReadScheduledBackupIgnoresTheStringInAScript(t *testing.T) {
	body := `apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: store
  namespace: prod
spec:
  mode: Prefix
  bucket: acme-backups
  note: |
    kind: ScheduledBackup
`
	_, err := readScheduledBackup(writeManifest(t, body))
	if !errors.Is(err, ErrNoScheduledBackup) {
		t.Errorf("expected ErrNoScheduledBackup, got %v", err)
	}
}

func normalized(s string) string {
	out, _ := normalizeScript(s)
	return out
}

func TestNormalizeScriptUnquotesEveryExcludeOnALine(t *testing.T) {
	original := `restic backup /data --exclude='cache' --exclude='tmp' --exclude='node_modules'`
	rendered := "restic backup --retry-lock=5m /data \\\n" +
		"  --exclude='cache' \\\n" +
		"  --exclude='tmp' \\\n" +
		"  --exclude='node_modules'"

	if got, want := normalized(original), normalized(rendered); got != want {
		t.Errorf("the same excludes normalized differently:\noriginal: %q\nrendered: %q", got, want)
	}
	if strings.Contains(normalized(original), "'") {
		t.Errorf("a quote survived normalization: %q", normalized(original))
	}
}

func TestNormalizeScriptStillSeesARealExcludeChange(t *testing.T) {
	a := `restic backup /data --exclude='cache' --exclude='tmp'`
	b := `restic backup /data --exclude='cache' --exclude='logs'`
	if normalized(a) == normalized(b) {
		t.Errorf("a changed exclude pattern normalized away: %q", normalized(a))
	}
}

func TestNormalizeScriptMatchesTheTwoCNPGDumpers(t *testing.T) {
	original := "restic backup --tag=db --stdin-from-command -- backup-cnpg.sh --dbname=app"
	rendered := "restic backup --retry-lock=5m --tag=db --stdin-from-command -- dumpdb cnpg " +
		"--secret-mount=/var/run/secrets/b2backup/database --database=app"

	a, aDumps := normalizeScript(original)
	b, bDumps := normalizeScript(rendered)
	if a != b {
		t.Errorf("the same dump normalized differently:\n%q\n%q", a, b)
	}
	if got := dumpChanges(aDumps, bDumps); len(got) != 1 || !strings.Contains(got[0], "<host>.dmp") {
		t.Errorf("dumpChanges = %q, want the format change named", got)
	}
	if got := dumpChanges(bDumps, bDumps); len(got) != 0 {
		t.Errorf("dumpChanges with nothing changed = %q", got)
	}

	other, _ := normalizeScript("restic backup --tag=db --stdin-from-command -- backup-cnpg.sh --dbname=other")
	if other == a {
		t.Error("a different database normalized away")
	}
}
