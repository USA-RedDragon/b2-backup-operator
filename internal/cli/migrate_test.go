package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

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
	err := runMigrate(&buf, writeMigrationDir(t, body), migrateOptions{repository: sopsRepoURL})
	return buf.String(), err
}

func migrated(t *testing.T, out string) (*b2backupv1.Repository, *b2backupv1.ScheduledBackup) {
	t.Helper()
	docs := strings.Split(out, "\n---\n")
	if len(docs) != 2 {
		t.Fatalf("expected two documents:\n%s", out)
	}
	var repo b2backupv1.Repository
	if err := yaml.Unmarshal([]byte(docs[0]), &repo); err != nil {
		t.Fatal(err)
	}
	var sb b2backupv1.ScheduledBackup
	if err := yaml.Unmarshal([]byte(docs[1]), &sb); err != nil {
		t.Fatal(err)
	}
	return &repo, &sb
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
			source, _, _, err := parseBackupLine(splitArgs(tt.line))
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
	_, _, _, err := parseBackupLine(splitArgs("restic backup /data --exclude"))
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

const (
	kustomizedNS = "myapp"
	mirrorPath   = "/mirror"
)

const homeClusterRelease = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: restic
  namespace: copied-from-elsewhere
spec:
  values:
    controllers:
      restic:
        type: cronjob
        cronjob:
          schedule: 32 0 * * *
          timeZone: America/Chicago
          concurrencyPolicy: Forbid
        containers:
          restic:
            workingDir: /
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
                restic backup --tag=db --stdin-from-command -- backup-cnpg.sh
                restic backup --tag=mirror /mirror
                restic forget --prune --keep-daily=90 --keep-weekly=24 --keep-monthly=24 --keep-yearly=20
                restic cache --cleanup
            resources:
              requests:
                cpu: 10m
                memory: 256Mi
      init:
        type: job
        containers:
          init:
            command: [sh, -c, "restic init || true"]
    persistence:
      postgresql-app:
        type: secret
        name: postgresql-app
      mirror:
        existingClaim: app-mirror
        advancedMounts:
          restic:
            restic:
              - path: /mirror
                readOnly: true
      unrelated:
        existingClaim: app-other
        advancedMounts:
          init:
            init:
              - path: /other
`

func writeHomeClusterRelease(t *testing.T, body string) string {
	t.Helper()
	dir := writeMigrationDir(t, body)
	kustomization := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: " + kustomizedNS + "\n"
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomization), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMigrateAdoptsTheExistingRepositoryInPrefixMode(t *testing.T) {
	var buf bytes.Buffer
	dir := writeHomeClusterRelease(t, homeClusterRelease)
	if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v\n%s", err, buf.String())
	}
	repo, sb := migrated(t, buf.String())

	if repo.Spec.Mode != b2backupv1.StorageModePrefix || repo.Spec.Bucket != testBucket ||
		repo.Spec.Path != sopsAppPath {
		t.Errorf("repository spec = %+v, want Prefix mode at acme-backups/restic/app", repo.Spec)
	}
	if ref := repo.Spec.PasswordSecretRef; ref == nil || ref.Name != "restic-envs" || ref.Key != "RESTIC_PASSWORD" {
		t.Errorf("passwordSecretRef = %+v, want restic-envs/RESTIC_PASSWORD", ref)
	}
	if repo.Namespace != kustomizedNS || sb.Namespace != kustomizedNS {
		t.Errorf("namespaces %q and %q, want the kustomization's myapp", repo.Namespace, sb.Namespace)
	}

	if v := sb.Spec.Volume; v == nil || v.ExistingClaim != "app-mirror" || v.MountPath != mirrorPath || !v.ReadOnly {
		t.Errorf("volume = %+v, want app-mirror read-only at /mirror", v)
	}
	if db := sb.Spec.Database; db == nil || db.Engine != b2backupv1.DatabaseEngineCNPG || db.SecretName != "postgresql-app" {
		t.Errorf("database = %+v, want cnpg from postgresql-app", db)
	}
	if len(sb.Spec.Sources) != 2 || sb.Spec.Sources[0].Type != b2backupv1.SourceTypeCNPG ||
		sb.Spec.Sources[1].Path != mirrorPath {
		t.Errorf("sources = %+v", sb.Spec.Sources)
	}
	if r := sb.Spec.Retention; r == nil || *r.Daily != 90 || *r.Yearly != 20 {
		t.Errorf("retention = %+v", r)
	}
	if sb.Spec.Resources == nil || sb.Spec.Resources.Requests.Memory().String() != "256Mi" {
		t.Errorf("resources = %+v, want the HelmRelease's", sb.Spec.Resources)
	}
	if len(sb.Spec.Env) != 0 || sb.Spec.ResticHost != "" {
		t.Errorf("env = %v, resticHost = %q; TZ and the namespace host are the operator's defaults",
			sb.Spec.Env, sb.Spec.ResticHost)
	}
	if !strings.Contains(buf.String(), "backup-cnpg.sh becomes dumpdb cnpg") {
		t.Errorf("the dump format change was not called out:\n%s", buf.String())
	}
}

func TestMigrateMountPathMatchesAppTemplate(t *testing.T) {
	var buf bytes.Buffer
	dir := writeHomeClusterRelease(t, strings.Replace(homeClusterRelease,
		"              - path: /mirror\n                readOnly: true\n", "              - readOnly: true\n", 1))
	if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v\n%s", err, buf.String())
	}
	_, sb := migrated(t, buf.String())
	if got := sb.Spec.Volume.EffectiveMountPath(); got != mirrorPath {
		t.Errorf("mount path = %q, want /mirror, after the persistence key", got)
	}
}

func TestMigrateNeedsTheRepositoryURL(t *testing.T) {
	err := runMigrate(&bytes.Buffer{}, writeHomeClusterRelease(t, homeClusterRelease), migrateOptions{})
	if !errors.Is(err, ErrNoRepositoryURL) {
		t.Fatalf("err = %v, want ErrNoRepositoryURL", err)
	}

	err = runMigrate(&bytes.Buffer{}, writeHomeClusterRelease(t, homeClusterRelease),
		migrateOptions{repository: "b2:" + testBucket + ":" + sopsAppPath})
	if !errors.Is(err, ErrNoRepositoryURL) {
		t.Fatalf("err = %v, want the native b2: URL refused", err)
	}
}

func TestMigrateReadsTheURLFromTheEncryptedSecret(t *testing.T) {
	secret := fakeSops(t, sopsEmitting("stringData:\n  RESTIC_REPOSITORY: "+sopsRepoURL+"\n"))
	var buf bytes.Buffer
	if err := runMigrate(&buf, writeHomeClusterRelease(t, homeClusterRelease),
		migrateOptions{fromSecret: secret}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo, _ := migrated(t, buf.String())
	if repo.Spec.Path != sopsAppPath {
		t.Errorf("path = %q, want restic/app from the decrypted Secret", repo.Spec.Path)
	}
}

func TestMigrateABucketRootRepositoryAdoptsTheBucket(t *testing.T) {
	var buf bytes.Buffer
	if err := runMigrate(&buf, writeHomeClusterRelease(t, homeClusterRelease),
		migrateOptions{repository: "s3:https://s3.us-east-005.backblazeb2.com/acme-app"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo, _ := migrated(t, buf.String())
	if repo.Spec.Mode != b2backupv1.StorageModeBucket || repo.Spec.Bucket != "acme-app" || repo.Spec.Path != "" {
		t.Errorf("spec = %+v, want Bucket mode on acme-app at the root", repo.Spec)
	}
	if !strings.Contains(buf.String(), "adopts the whole") {
		t.Errorf("the whole-bucket consequence of Delete was not called out:\n%s", buf.String())
	}
}

func TestMigrateKeepsAnInexpressibleScriptVerbatim(t *testing.T) {
	body := strings.Replace(homeClusterRelease,
		"restic backup --tag=db --stdin-from-command -- backup-cnpg.sh\n",
		"restic backup --tag=db --tag=app --stdin-filename=app.sql --stdin-from-command -- backup-cnpg.sh --dbname=app\n", 1)
	body = strings.Replace(body, "              TZ: America/Chicago\n",
		"              TZ: America/Chicago\n              CNPG_SECRET_PATH: /postgresql-app\n", 1)

	var buf bytes.Buffer
	if err := runMigrate(&buf, writeHomeClusterRelease(t, body), migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, buf.String())

	if sb.Spec.Sources != nil || sb.Spec.Retention != nil {
		t.Errorf("sources %+v and retention %+v emitted beside a script", sb.Spec.Sources, sb.Spec.Retention)
	}
	if !strings.Contains(sb.Spec.Script, "--stdin-filename=app.sql") || strings.Contains(sb.Spec.Script, "set -eu") ||
		strings.Contains(sb.Spec.Script, "cache --cleanup") {
		t.Errorf("script = %q, want the body without the operator's own preamble and postamble", sb.Spec.Script)
	}
	if got := sb.Spec.Env["CNPG_SECRET_PATH"]; got != b2backupv1.DBSecretMountPath {
		t.Errorf("CNPG_SECRET_PATH = %q, want it pointed at the operator's mount", got)
	}
	if sb.Spec.Database == nil || sb.Spec.Database.SecretName != "postgresql-app" {
		t.Errorf("database = %+v, want the secret still mounted", sb.Spec.Database)
	}
}

func TestMigrateRefusesAnEmptyDir(t *testing.T) {
	body := homeClusterRelease + `      scratch:
        type: emptyDir
        globalMounts:
          - path: /backup
`
	err := runMigrate(&bytes.Buffer{}, writeHomeClusterRelease(t, body), migrateOptions{repository: sopsRepoURL})
	if !errors.Is(err, ErrUnsupportedMount) || !strings.Contains(err.Error(), "/backup") {
		t.Fatalf("err = %v, want ErrUnsupportedMount naming /backup", err)
	}
}

func TestMigrateCopiesAffinityAndDerivesTheCNPGCluster(t *testing.T) {
	body := strings.Replace(homeClusterRelease, "spec:\n  values:\n", `spec:
  values:
    defaultPodOptions:
      affinity:
        podAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - topologyKey: kubernetes.io/hostname
              labelSelector:
                matchLabels:
                  app.kubernetes.io/name: grafana
`, 1)
	body = strings.Replace(body, "name: postgresql-app\n", "name: obsidibot-app\n", 1)

	var buf bytes.Buffer
	if err := runMigrate(&buf, writeHomeClusterRelease(t, body), migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, buf.String())
	if a := sb.Spec.Affinity; a == nil || a.PodAffinity == nil ||
		a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchLabels["app.kubernetes.io/name"] != "grafana" {
		t.Errorf("affinity = %+v, want the HelmRelease's", a)
	}
	if db := sb.Spec.Database; db.SecretName != "obsidibot-app" || db.CNPGCluster != "obsidibot" {
		t.Errorf("database = %+v, want obsidibot-app in cluster obsidibot", db)
	}
}

func TestMigrateAcceptsAFileAndNamesTheBackupAfterTheRelease(t *testing.T) {
	dir := writeHomeClusterRelease(t, strings.Replace(homeClusterRelease, "name: restic\n", "name: obsidibot-restic\n", 1))
	file := filepath.Join(dir, "restic.yaml")
	if err := os.Rename(filepath.Join(dir, "helmrelease.yaml"), file); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runMigrate(&buf, file, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo, sb := migrated(t, buf.String())
	if sb.Name != "obsidibot-restic" || repo.Name != "restic" || sb.Spec.RepositoryRef.Name != "restic" {
		t.Errorf("names %s and %s, want obsidibot-restic backing up into restic", sb.Name, repo.Name)
	}
}

func TestMigrateThenParityOnAHomeClusterRelease(t *testing.T) {
	dir := writeHomeClusterRelease(t, homeClusterRelease)
	var buf bytes.Buffer
	if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	generated := filepath.Join(t.TempDir(), "generated.yaml")
	if err := os.WriteFile(generated, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runParity(&out, generated, dir)
	if !errors.Is(err, ErrParityDiffers) {
		t.Fatalf("parity = %v, want the dump change reported\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "DUMP FORMAT CHANGES") || strings.Contains(got, "SCRIPT DIFFERS") ||
		strings.Contains(got, "VOLUME DIFFERS") || strings.Contains(got, "DATABASE SECRET DIFFERS") {
		t.Errorf("want only the dump change, got:\n%s", got)
	}
	if !strings.Contains(got, "RESCHEDULED") {
		t.Errorf("want the schedule reported as moved:\n%s", got)
	}
}

func TestMigrateThenParityOnAVerbatimScriptIsEquivalent(t *testing.T) {
	body := strings.Replace(homeClusterRelease,
		"restic backup --tag=db --stdin-from-command -- backup-cnpg.sh\n",
		"restic backup --tag=db --tag=app --stdin-filename=app.sql --stdin-from-command -- backup-cnpg.sh --dbname=app\n", 1)
	dir := writeHomeClusterRelease(t, body)
	var buf bytes.Buffer
	if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	generated := filepath.Join(t.TempDir(), "generated.yaml")
	if err := os.WriteFile(generated, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runParity(&out, generated, dir); err != nil {
		t.Fatalf("parity: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "EQUIVALENT") {
		t.Errorf("want EQUIVALENT:\n%s", out.String())
	}
}
