package cli_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"
	"sigs.k8s.io/yaml"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
)

const (
	zoneRepository = "s3:https://s3.us-east-005.backblazeb2.com/acme-backups/k8s/media"
	zoneChicago    = "America/Chicago"
	zoneUTC        = "UTC"
	zoneDiffers    = "TIMEZONE DIFFERS"
	zoneEnvDiffers = "ENV DIFFERS"
	zoneDaily      = "0 0 * * *"
	zoneDST        = "DAYLIGHT SAVING"
)

const zoneRelease = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: restic
  namespace: media
spec:
  chartRef:
    kind: OCIRepository
    name: app-template
    namespace: flux-system
  values:
    defaultPodOptions:
      affinity:
        podAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - topologyKey: kubernetes.io/hostname
              labelSelector:
                matchLabels:
                  app.kubernetes.io/name: media
    controllers:
      restic:
        type: cronjob
        cronjob:
          schedule: SCHEDULE
TIMEZONE          concurrencyPolicy: Forbid
        containers:
          restic:
            image:
              repository: ghcr.io/gabe565/restic
              tag: 0.18.1
            workingDir: /data
            envFrom:
              - secretRef:
                  name: restic-b2
              - secretRef:
                  name: restic-envs
            env:
TZENV              RESTIC_HOST:
                fieldRef:
                  fieldPath: metadata.namespace
            command:
              - sh
              - -c
              - |
                set -eu
                restic backup --tag=files /data
                restic forget --prune --keep-daily=90 --keep-weekly=24
            resources:
              requests:
                cpu: 100m
                memory: 256Mi
    persistence:
      data:
        existingClaim: media-config
`

func zoneReleaseDir(t *testing.T, schedule, cronJobZone, containerTZ string) string {
	t.Helper()
	body := strings.Replace(zoneRelease, "SCHEDULE", schedule, 1)
	line := ""
	if cronJobZone != "" {
		line = "          timeZone: " + cronJobZone + "\n"
	}
	body = strings.Replace(body, "TIMEZONE", line, 1)
	line = ""
	if containerTZ != "" {
		line = "              TZ: " + containerTZ + "\n"
	}
	body = strings.Replace(body, "TZENV", line, 1)
	dir := filepath.Join(t.TempDir(), "resources", "restic")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "helmrelease.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runB2bk(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := cli.New(genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: &out, ErrOut: &errOut}, "b2bk")
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	return out.String() + errOut.String(), err
}

func migrateZone(t *testing.T, dir string) (string, string) {
	t.Helper()
	out, err := runB2bk(t, "migrate", dir, "--repository", zoneRepository)
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	generated := filepath.Join(t.TempDir(), "generated.yaml")
	if err := os.WriteFile(generated, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	return generated, out
}

func generatedTimeZone(t *testing.T, out string) string {
	t.Helper()
	for doc := range strings.SplitSeq(out, "\n---\n") {
		var obj struct {
			Kind string `json:"kind"`
			Spec struct {
				TimeZone string `json:"timeZone"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("reading the migrate output: %v", err)
		}
		if obj.Kind == "ScheduledBackup" {
			return obj.Spec.TimeZone
		}
	}
	t.Fatalf("no ScheduledBackup in:\n%s", out)
	return ""
}

func checkParity(t *testing.T, out string, err error, wantDiffers bool, want, notWant []string) {
	t.Helper()
	if differs := err != nil; differs != wantDiffers {
		t.Errorf("parity err = %v, want differences %t:\n%s", err, wantDiffers, out)
	}
	if err != nil && !errors.Is(err, cli.ErrParityDiffers) {
		t.Errorf("parity err = %v, want ErrParityDiffers", err)
	}
	for _, s := range want {
		if !strings.Contains(out, s) {
			t.Errorf("parity output lacks %q:\n%s", s, out)
		}
	}
	for _, s := range notWant {
		if strings.Contains(out, s) {
			t.Errorf("parity output has %q:\n%s", s, out)
		}
	}
}

func TestMigrateTakesTheZoneForACronJobWithoutTimeZone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		schedule    string
		cronJobZone string
		containerTZ string
		wantZone    string
		wantDiffers bool
		want        []string
		notWant     []string
	}{
		{
			name:        "the container's TZ, under a jittered schedule",
			schedule:    zoneDaily,
			containerTZ: zoneChicago,
			wantZone:    zoneChicago,
			want: []string{`RESCHEDULED: "0 0 * * *" (daily at 00:00 UTC) -> `, " America/Chicago) (",
				"it now runs in America/Chicago instead of UTC", "EQUIVALENT"},
			notWant: []string{zoneDiffers, zoneEnvDiffers},
		},
		{
			name:        "the container's TZ, under a pinned schedule",
			schedule:    "0 3 * * 0",
			containerTZ: zoneChicago,
			wantZone:    zoneChicago,
			wantDiffers: true,
			want:        []string{"TIMEZONE DIFFERS: original UTC, generated America/Chicago"},
			notWant:     []string{zoneEnvDiffers},
		},
		{
			name:     "UTC without a TZ",
			schedule: zoneDaily,
			wantZone: zoneUTC,
			want:     []string{"(daily at 00:00 UTC) -> ", "EQUIVALENT"},
			notWant:  []string{zoneDiffers, zoneEnvDiffers, zoneDST},
		},
		{
			name:        "the CronJob's own zone over the TZ",
			schedule:    zoneDaily,
			cronJobZone: "Europe/Berlin",
			containerTZ: zoneChicago,
			wantZone:    "Europe/Berlin",
			wantDiffers: true,
			want:        []string{"ENV DIFFERS: original TZ=America/Chicago"},
			notWant:     []string{zoneDiffers},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := zoneReleaseDir(t, tt.schedule, tt.cronJobZone, tt.containerTZ)
			generated, migrateOut := migrateZone(t, dir)
			if got := generatedTimeZone(t, migrateOut); got != tt.wantZone {
				t.Errorf("timeZone = %q, want %q:\n%s", got, tt.wantZone, migrateOut)
			}
			out, err := runB2bk(t, "parity", generated, dir, "--repository", zoneRepository)
			checkParity(t, out, err, tt.wantDiffers, tt.want, tt.notWant)
		})
	}
}

func TestParityReportsDaylightSavingInTheGeneratedZone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		original    string
		generated   string
		wantDiffers bool
		want        []string
		notWant     []string
	}{
		{
			name:      "moved into the hour the clocks repeat",
			original:  zoneDaily,
			generated: "0 1 * * *",
			want: []string{"DAYLIGHT SAVING: original fires every day, generated fires twice at 01:00 America/Chicago on 1 day(s)",
				"it now runs in America/Chicago instead of UTC"},
			notWant: []string{zoneDiffers},
		},
		{
			name:        "kept in the hour the clocks skip",
			original:    "30 2 * * *",
			generated:   "30 2 * * *",
			wantDiffers: true,
			want: []string{"TIMEZONE DIFFERS: original UTC, generated America/Chicago",
				"generated skips 02:30 America/Chicago on 1 day(s)"},
		},
		{
			name:      "moved to an hour every day has",
			original:  zoneDaily,
			generated: "17 0 * * *",
			notWant:   []string{zoneDiffers, zoneDST},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := zoneReleaseDir(t, tt.original, "", zoneChicago)
			generated, migrateOut := migrateZone(t, dir)
			pinned := strings.Replace(migrateOut, "schedule: '@daily'", "schedule: "+tt.generated, 1)
			if pinned == migrateOut {
				t.Fatalf("no @daily schedule in:\n%s", migrateOut)
			}
			if err := os.WriteFile(generated, []byte(pinned), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runB2bk(t, "parity", generated, dir, "--repository", zoneRepository)
			checkParity(t, out, err, tt.wantDiffers, tt.want, tt.notWant)
		})
	}
}
