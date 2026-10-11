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
metadata:
  namespace: prod
spec:
  values:
    controllers:
      restic:
        type: cronjob
        cronjob:
          schedule: "` + schedule + `"
          timeZone: America/Chicago
        containers:
          restic:
            image:
              repository: ghcr.io/gabe565/restic
              tag: 0.19.1
            envFrom:
              - secretRef:
                  name: restic-b2
              - secretRef:
                  name: restic-envs
            env:
              TZ: America/Chicago
              RESTIC_HOST:
                fieldRef:
                  fieldPath: metadata.namespace
            resources:
              requests:
                cpu: 10m
                memory: 256Mi
              limits:
                memory: 1Gi
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
  resources:
    requests: {cpu: 10m, memory: 256Mi}
    limits: {memory: 1Gi}
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
	err := runParity(&buf, writeGenerated(t, generated), writeHelmRelease(t, original), migrateOptions{})
	if err != nil {
		t.Log(buf.String())
	}
	return buf.String(), err
}

func TestParityAcceptsAReJitteredSchedule(t *testing.T) {
	out, err := parity(t, "@hourly", "36 * * * *")
	if err != nil {
		t.Fatalf("a re-jittered schedule should pass: %v\n%s", err, out)
	}
	for _, want := range []string{"RESCHEDULED", verdictEquivalent} {
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

func TestParityDoesNotRescheduleWhenNothingMoved(t *testing.T) {
	out, err := parity(t, "36 * * * *", "36 * * * *")
	if err != nil {
		t.Fatalf("unexpected difference: %v\n%s", err, out)
	}
	if !strings.Contains(out, verdictEquivalent) {
		t.Errorf("expected EQUIVALENT in output:\n%s", out)
	}
	if strings.Contains(out, "RESCHEDULED") {
		t.Errorf("nothing moved, so nothing should be reported as rescheduled:\n%s", out)
	}
}

func TestParityIsIdenticalAgainstTheOperatorsOwnCronJob(t *testing.T) {
	t.Parallel()
	generated := writeGenerated(t, "36 * * * *")
	cj, _, _, err := generatedCronJob(generated)
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cj)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(t.TempDir(), "cronjob.yaml")
	if err := os.WriteFile(original, append([]byte("apiVersion: batch/v1\nkind: CronJob\n"), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runParity(&out, generated, original, migrateOptions{}); err != nil || strings.TrimSpace(out.String()) != "IDENTICAL" {
		t.Errorf("parity against the operator's own CronJob = %v:\n%s", err, out.String())
	}
}

func TestParityRefusesAGeneratedBackupTheOperatorRejects(t *testing.T) {
	t.Parallel()
	generated := writeManifest(t, `apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: web-files
  namespace: prod
spec:
  repositoryRef: {name: store}
  schedule: "36 * * * *"
  env: {DB_HOST: elsewhere}
  database: {engine: mariadb, host: db, name: app, user: app}
  sources:
    - type: mariadb
`)
	err := runParity(&bytes.Buffer{}, generated, writeHelmRelease(t, "36 * * * *"), migrateOptions{})
	if err == nil || !strings.Contains(err.Error(), "DB_HOST") {
		t.Errorf("parity = %v, want the operator's refusal of DB_HOST in spec.env", err)
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
  volumes:
    - existingClaim: data
  sources:
    - type: files
      paths: [/data]
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
  volumes:
    - existingClaim: data
  sources:
    - type: files
      paths: [/data]
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
  volumes:
    - existingClaim: data
  sources:
    - type: files
      paths: [/data]
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

func TestDescribeScheduleNamesTheTimeOfDay(t *testing.T) {
	t.Parallel()
	tests := []struct{ expr, want string }{
		{"44 0 * * *", `"44 0 * * *" (daily at 00:44 America/Chicago)`},
		{"7 15 * * *", `"7 15 * * *" (daily at 15:07 America/Chicago)`},
		{"36 * * * *", `"36 * * * *" (hourly at :36)`},
		{"0 */6 * * *", `"0 */6 * * *"`},
	}
	for _, tt := range tests {
		if got := describeSchedule(tt.expr, defaultTimeZone); got != tt.want {
			t.Errorf("describeSchedule(%q) = %s, want %s", tt.expr, got, tt.want)
		}
	}
}

const parityRelease = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: restic
  namespace: app
spec:
  values:
    defaultPodOptions:
      affinity:
        podAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - topologyKey: kubernetes.io/hostname
              labelSelector:
                matchLabels:
                  app.kubernetes.io/name: app
    controllers:
      restic:
        type: cronjob
        cronjob:
          schedule: 44 0 * * *
          timeZone: America/Chicago
        containers:
          restic:
            image:
              repository: ghcr.io/gabe565/restic
              tag: 0.19.1
            envFrom:
              - secretRef:
                  name: restic-b2
              - secretRef:
                  name: restic-envs
            workingDir: /data
            resources:
              requests:
                cpu: 10m
                memory: 256Mi
              limits:
                memory: 1Gi
            env:
              TZ: America/Chicago
              RESTIC_HOST:
                fieldRef:
                  fieldPath: metadata.namespace
            command:
              - sh
              - -c
              - |
                exec > >(ts '%H:%M:%S') 2>&1
                set -eu
                restic backup --tag=files /data
                restic forget --prune --keep-daily=7
                restic cache --cleanup
    persistence:
      data:
        existingClaim: app-data
`

const parityGenerated = `apiVersion: b2backup.mcswain.dev/v1
kind: Repository
metadata:
  name: restic
  namespace: app
spec:
  mode: Prefix
  bucket: acme-backups
  path: restic/app
---
apiVersion: b2backup.mcswain.dev/v1
kind: ScheduledBackup
metadata:
  name: restic
  namespace: app
spec:
  repositoryRef: {name: restic}
  schedule: "@daily"
  timeZone: America/Chicago
  affinity:
    podAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - topologyKey: kubernetes.io/hostname
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: app
  resources:
    requests: {cpu: 10m, memory: 256Mi}
    limits: {memory: 1Gi}
  volumes:
    - existingClaim: app-data
      mountPath: /data
  sources:
    - type: files
      paths: [/data]
  retention: {daily: 7}
`

func TestParityComparesEverythingThatDecidesWhereABackupGoes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		generated func(string) string
		opts      migrateOptions
		want      string
	}{
		{"nothing but the time moved", func(s string) string { return s }, migrateOptions{repository: sopsRepoURL},
			verdictEquivalent},
		{"the working directory", func(s string) string {
			return strings.Replace(s, "  volumes:", "  workingDir: /\n  volumes:", 1)
		}, migrateOptions{}, "WORKING DIR DIFFERS"},
		{"the snapshot host", func(s string) string {
			return strings.Replace(s, "  volumes:", "  resticHost: elsewhere\n  volumes:", 1)
		}, migrateOptions{}, "RESTIC HOST DIFFERS"},
		{"an extra variable", func(s string) string {
			return strings.Replace(s, "  volumes:", "  env: {RESTIC_GROUP_BY: host}\n  volumes:", 1)
		}, migrateOptions{}, "ENV DIFFERS"},
		{"the repository path", func(s string) string {
			return strings.Replace(s, "path: restic/app", "path: restic/other", 1)
		}, migrateOptions{repository: sopsRepoURL}, "REPOSITORY DIFFERS"},
		{"a second claim", func(s string) string {
			return strings.Replace(s, "      mountPath: /data\n", "      mountPath: /data\n    - existingClaim: more\n", 1)
		}, migrateOptions{}, volumeDiffers},
		{"a read-only mount", func(s string) string {
			return strings.Replace(s, "      mountPath: /data\n", "      mountPath: /data\n      readOnly: true\n", 1)
		}, migrateOptions{}, volumeDiffers},
		{"scratch space", func(s string) string {
			return strings.Replace(s, "  volumes:", "  scratch: {mountPath: /backup}\n  volumes:", 1)
		}, migrateOptions{}, volumeDiffers},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			hr := filepath.Join(dir, "helmrelease.yaml")
			gen := filepath.Join(dir, "generated.yaml")
			if err := os.WriteFile(hr, []byte(parityRelease), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(gen, []byte(tt.generated(strings.ReplaceAll(parityGenerated,
				"acme-backups", testBucket))), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := runParity(&out, gen, hr, tt.opts)
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("want %s in:\n%s", tt.want, out.String())
			}
			if tt.want == verdictEquivalent {
				if err != nil {
					t.Errorf("parity = %v", err)
				}
				for _, note := range []string{"FORGET SCOPED", "daily at 00:44 America/Chicago"} {
					if !strings.Contains(out.String(), note) {
						t.Errorf("want %q noted in:\n%s", note, out.String())
					}
				}
			} else if !errors.Is(err, ErrParityDiffers) {
				t.Errorf("parity = %v, want ErrParityDiffers", err)
			}
		})
	}
}

func TestNormalizeRenderedKeepsAForgetScopedToOtherTags(t *testing.T) {
	t.Parallel()
	rendered := `restic forget --prune --host="$RESTIC_HOST" --group-by=tags --tag=files --keep-daily=7`
	if got, _, note := normalizeRendered(rendered, []string{filesTag}); got != "restic forget --prune --keep-daily=7" ||
		note == "" {
		t.Errorf("normalized = %q, note %q", got, note)
	}
	if got, _, _ := normalizeRendered(rendered, []string{"db"}); !strings.Contains(got, "--tag=files") {
		t.Errorf("a forget scoped to tags the backup does not take was normalized away: %q", got)
	}
}

func TestParityComparesTheWholePodTheOriginalRuns(t *testing.T) {
	t.Parallel()
	const pod = "    controllers:\n      restic:\n        type: cronjob\n"
	const container = "            workingDir: /data\n"
	tests := []struct {
		name, old, new, want string
	}{
		{"a node selector", pod, pod + "        pod:\n          nodeSelector: {disk: ssd}\n", "NODE SELECTOR DIFFERS"},
		{"tolerations", pod, pod + "        pod:\n          tolerations: [{key: backup, operator: Exists}]\n",
			"TOLERATIONS DIFFER"},
		{"a priority class", pod, pod + "        pod:\n          priorityClassName: low\n", "PRIORITY CLASS DIFFERS"},
		{"a pod label", pod, pod + "        pod:\n          labels: {team: storage}\n", "POD LABELS DIFFER"},
		{"suspend", "          timeZone: America/Chicago\n", "          timeZone: America/Chicago\n          suspend: true\n",
			"SUSPEND DIFFERS"},
		{"another image", "repository: ghcr.io/gabe565/restic", "repository: docker.io/restic/restic", "IMAGE DIFFERS"},
		{"another envFrom Secret", "                  name: restic-envs\n",
			"                  name: restic-envs\n              - secretRef:\n                  name: app-extra\n", "ENVFROM DIFFERS"},
		{"an init container", pod, pod + "        initContainers:\n          prepare:\n            image: {repository: busybox, tag: '1'}\n" +
			"            command: [sh, -c, 'true']\n", "CONTAINERS DIFFER"},
		{"a sidecar", "        containers:\n", "        containers:\n          sidecar:\n" +
			"            image: {repository: busybox, tag: '1'}\n            args: [sleep, '60']\n", "CONTAINERS DIFFER"},
		{"another controller", "    controllers:\n", "    controllers:\n      notify:\n        type: job\n        containers:\n" +
			"          notify:\n            image: {repository: busybox, tag: '1'}\n            command: [sh, -c, 'curl x']\n",
			"CONTROLLER DROPPED"},
		{"a container port", container, container + "            ports: [{containerPort: 80}]\n", "CONTAINER DIFFERS"},
		{"host networking", pod, pod + "        pod:\n          hostNetwork: true\n", "POD SPEC DIFFERS"},
		{"a time limit", "          timeZone: America/Chicago\n",
			"          timeZone: America/Chicago\n          activeDeadlineSeconds: 600\n", "JOB SETTINGS DIFFER"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(parityRelease, tt.old) {
				t.Fatalf("fixture has no %q", tt.old)
			}
			dir := t.TempDir()
			hr := filepath.Join(dir, "helmrelease.yaml")
			gen := filepath.Join(dir, "generated.yaml")
			if err := os.WriteFile(hr, []byte(strings.Replace(parityRelease, tt.old, tt.new, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(gen, []byte(parityGenerated), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := runParity(&out, gen, hr, migrateOptions{})
			if !errors.Is(err, ErrParityDiffers) || !strings.Contains(out.String(), tt.want) {
				t.Errorf("parity = %v, want %s in:\n%s", err, tt.want, out.String())
			}
		})
	}
}

func TestParityCatchesResticsCacheBackedUpInPlaceOfAClaim(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hr := filepath.Join(dir, "helmrelease.yaml")
	gen := filepath.Join(dir, "generated.yaml")
	original := strings.Replace(parityRelease, "restic backup --tag=files /data", "restic backup --tag=files /cache", 1) +
		"      cache:\n        existingClaim: app-cache\n"
	generated := strings.Replace(parityGenerated, "  sources:\n    - type: files\n      paths: [/data]\n  retention: {daily: 7}\n",
		"  cache: {}\n  script: |\n    restic backup --tag=files /cache\n    restic forget --prune --keep-daily=7\n", 1)
	if err := os.WriteFile(hr, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gen, []byte(generated), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runParity(&out, gen, hr, migrateOptions{})
	if !errors.Is(err, ErrParityDiffers) || !strings.Contains(out.String(), volumeDiffers+": original app-cache at /cache") {
		t.Errorf("parity = %v; restic's own cache backed up in place of app-cache must differ:\n%s", err, out.String())
	}
}
