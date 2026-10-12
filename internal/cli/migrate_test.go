package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/utils/ptr"
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
    defaultPodOptions:
      affinity:
        podAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - topologyKey: kubernetes.io/hostname
              labelSelector:
                matchLabels:
                  app.kubernetes.io/name: prod
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
            workingDir: /data
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
	out, err := migrate(t, helmReleaseWith("0 */5 * * *", oneClaim, backupData))
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

func TestMigrateMountsEveryClaim(t *testing.T) {
	t.Parallel()
	persistence := `      uploads:
        existingClaim: app-uploads
      data:
        existingClaim: app-data
`
	script := "restic backup --tag=server /data\nrestic backup --tag=uploads /uploads\n" +
		"restic forget --prune --keep-daily=7"
	out, err := migrate(t, helmReleaseWith("36 * * * *", persistence, script))
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	_, sb := migrated(t, out)
	if len(sb.Spec.Volumes) != 2 || sb.Spec.Volumes[0].ExistingClaim != "app-data" ||
		sb.Spec.Volumes[1].ExistingClaim != "app-uploads" {
		t.Errorf("volumes = %+v, want app-data, the working directory's claim, first", sb.Spec.Volumes)
	}
	if sb.Spec.WorkingDir != "" {
		t.Errorf("workingDir = %q, want it left to the default, /data", sb.Spec.WorkingDir)
	}
	if len(sb.Spec.Sources) != 2 || sb.Spec.Sources[1].Paths[0] != "/uploads" {
		t.Errorf("sources = %+v", sb.Spec.Sources)
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
	out, err := migrate(t, helmReleaseWith("36 * * * *", oneClaim, backupData))
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "existingClaim: app-data") {
		t.Errorf("expected the claim in the output:\n%s", out)
	}
}

func TestParseBackupLineReadsSpaceSeparatedExcludes(t *testing.T) {
	const wantPath = dataDir
	bothExcludes := []string{cacheDir, "tmp"}

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
			wantExcl: []string{cacheDir},
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
			if len(source.Paths) != 1 || source.Paths[0] != wantPath {
				t.Errorf("paths = %q, want %q", source.Paths, wantPath)
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

func TestMigrateKeepsARelativePathAsAScript(t *testing.T) {
	t.Parallel()
	out, err := migrate(t, helmReleaseWith("36 * * * *", oneClaim, "restic backup ."))
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	_, sb := migrated(t, out)
	if sb.Spec.Sources != nil || !strings.Contains(sb.Spec.Script, "restic backup .") {
		t.Errorf("a relative path became a source; restic stores it with another tree layout: %+v", sb.Spec)
	}
}

func TestMigrateMountsTheClaimTheScriptBacksUp(t *testing.T) {
	out, err := migrate(t, helmReleaseWith("36 * * * *", oneClaim, backupData))
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
	if len(sb.Spec.Volumes) != 1 {
		t.Fatalf("volumes = %+v, want one", sb.Spec.Volumes)
	}
	if v := sb.Spec.Volumes[0]; v.ExistingClaim != "app-data" || v.MountPath != dataDir {
		t.Errorf("mounted %q at %q, want app-data at /data", v.ExistingClaim, v.MountPath)
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
    defaultPodOptions:
      affinity:
        podAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - topologyKey: kubernetes.io/hostname
              labelSelector:
                matchLabels:
                  app.kubernetes.io/name: myapp
    controllers:
      restic:
        type: cronjob
        cronjob:
          schedule: 32 0 * * *
          timeZone: America/Chicago
          concurrencyPolicy: Forbid
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

	if v := sb.Spec.Volumes; len(v) != 1 || v[0].ExistingClaim != "app-mirror" || v[0].MountPath != mirrorPath ||
		!v[0].ReadOnly {
		t.Errorf("volumes = %+v, want app-mirror read-only at /mirror", v)
	}
	if sb.Spec.WorkingDir != "/" {
		t.Errorf("workingDir = %q, want the HelmRelease's /", sb.Spec.WorkingDir)
	}
	if db := sb.Spec.Database; db == nil || db.Engine != b2backupv1.DatabaseEngineCNPG || db.SecretName != "postgresql-app" {
		t.Errorf("database = %+v, want cnpg from postgresql-app", db)
	}
	if len(sb.Spec.Sources) != 2 || sb.Spec.Sources[0].Type != b2backupv1.SourceTypeCNPG ||
		len(sb.Spec.Sources[1].Paths) != 1 || sb.Spec.Sources[1].Paths[0] != mirrorPath {
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

func TestMigrateLeavesLifecycleAndDeletionPolicyAlone(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := runMigrate(&buf, writeHomeClusterRelease(t, homeClusterRelease), migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v\n%s", err, buf.String())
	}
	repo, _ := migrated(t, buf.String())
	if repo.Spec.Lifecycle != nil || repo.Spec.DeletionPolicy != "" {
		t.Errorf("lifecycle %+v, deletionPolicy %q: migrate must leave the bucket's own rules in charge "+
			"and the default Retain", repo.Spec.Lifecycle, repo.Spec.DeletionPolicy)
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
	if got := sb.Spec.Volumes[0].EffectiveMountPath(); got != mirrorPath {
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
	secret := fakeSops(t, sopsEmitting("stringData:\n  RESTIC_REPOSITORY: "+sopsRepoURL+"\n  RESTIC_PASSWORD: p\n"))
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

func TestMigrateTurnsAnEmptyDirIntoScratchSpace(t *testing.T) {
	t.Parallel()
	body := homeClusterRelease + `      scratch:
        type: emptyDir
        sizeLimit: 2Gi
        globalMounts:
          - path: /backup
`
	var buf bytes.Buffer
	if err := runMigrate(&buf, writeHomeClusterRelease(t, body), migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, buf.String())
	if sc := sb.Spec.Scratch; sc == nil || sc.MountPath != backupDir || sc.SizeLimit == nil || sc.SizeLimit.String() != "2Gi" {
		t.Errorf("scratch = %+v, want 2Gi at /backup", sc)
	}

	twice := body + `      more:
        type: emptyDir
        globalMounts:
          - path: /more
`
	err := runMigrate(&bytes.Buffer{}, writeHomeClusterRelease(t, twice), migrateOptions{repository: sopsRepoURL})
	if !errors.Is(err, ErrUnsupportedMount) {
		t.Errorf("err = %v, want two emptyDirs refused", err)
	}
}

func TestMigrateCopiesAffinityAndDerivesTheCNPGCluster(t *testing.T) {
	body := strings.Replace(homeClusterRelease, "app.kubernetes.io/name: myapp", "app.kubernetes.io/name: grafana", 1)
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
	err := runParity(&out, generated, dir, migrateOptions{repository: sopsRepoURL})
	if !errors.Is(err, ErrParityDiffers) {
		t.Fatalf("parity = %v, want the dump change reported\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "DUMP FORMAT CHANGES") || strings.Contains(got, "SCRIPT DIFFERS") ||
		strings.Contains(got, volumeDiffers) || strings.Contains(got, "DATABASE SECRET DIFFERS") ||
		strings.Contains(got, "ENV DIFFERS") {
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
	if err := runParity(&out, generated, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("parity: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), verdictEquivalent) {
		t.Errorf("want EQUIVALENT:\n%s", out.String())
	}
}

func TestMigrateKeepsTheReleasesProgressFPS(t *testing.T) {
	body := strings.Replace(homeClusterRelease, "              TZ: America/Chicago\n",
		"              TZ: America/Chicago\n              "+backup.EnvProgressFPS+": \"2\"\n", 1)
	dir := writeHomeClusterRelease(t, body)
	var buf bytes.Buffer
	if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(buf.String(), backup.EnvProgressFPS+`: "2"`) {
		t.Errorf("migrate did not carry %s into spec.env:\n%s", backup.EnvProgressFPS, buf.String())
	}
	generated := filepath.Join(t.TempDir(), "generated.yaml")
	if err := os.WriteFile(generated, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_ = runParity(&out, generated, dir, migrateOptions{repository: sopsRepoURL})
	if strings.Contains(out.String(), "ENV DIFFERS") {
		t.Errorf("parity reports the carried value as a difference:\n%s", out.String())
	}
}

func TestMigrateTheShapesTheHomeClusterUses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		workingDir  string
		persistence string
		script      string
		check       func(t *testing.T, sb *b2backupv1.ScheduledBackup)
	}{
		{
			name:       "two claims, two sources",
			workingDir: dataDir,
			persistence: `      data:
        existingClaim: minecraft
      filebrowser:
        existingClaim: filebrowser
`,
			script: "restic backup --tag=server /data\nrestic backup --tag=filebrowser /filebrowser\n" +
				"restic forget --prune --keep-daily=90",
			check: func(t *testing.T, sb *b2backupv1.ScheduledBackup) {
				if len(sb.Spec.Volumes) != 2 || len(sb.Spec.Sources) != 2 || sb.Spec.Sources[1].Tag != filebrowserTag {
					t.Errorf("spec = %+v", sb.Spec)
				}
			},
		},
		{
			name:       "two claims in one snapshot",
			workingDir: dataDir,
			persistence: `      data:
        existingClaim: config
      blobs:
        existingClaim: blobs
`,
			script: "restic backup --tag=files /data /blobs\nrestic forget --prune --keep-daily=90",
			check: func(t *testing.T, sb *b2backupv1.ScheduledBackup) {
				if len(sb.Spec.Sources) != 1 || !slices.Equal(sb.Spec.Sources[0].Paths, []string{dataDir, blobsDir}) {
					t.Errorf("sources = %+v, want one snapshot of both paths", sb.Spec.Sources)
				}
				if sb.Spec.Volumes[0].ExistingClaim != "config" {
					t.Errorf("volumes = %+v, want the working directory's claim first", sb.Spec.Volumes)
				}
			},
		},
		{
			name:       "a staged copy in an emptyDir",
			workingDir: backupDir,
			persistence: `      data:
        existingClaim: wx
        globalMounts:
          - path: /data
            readOnly: true
      backup:
        type: emptyDir
        globalMounts:
          - path: /backup
`,
			script: "sqlite3 -readonly -bail /data/wx.db \".backup /backup/wx.db\"\n" +
				"restic backup --tag=sqlite /backup/wx.db\nrestic forget --prune --keep-daily=90",
			check: func(t *testing.T, sb *b2backupv1.ScheduledBackup) {
				if sb.Spec.Script == "" || sb.Spec.Scratch == nil || sb.Spec.Scratch.MountPath != backupDir ||
					sb.Spec.WorkingDir != backupDir || !sb.Spec.Volumes[0].ReadOnly {
					t.Errorf("spec = %+v", sb.Spec)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Replace(helmReleaseWith("44 0 * * *", tt.persistence, tt.script+"\nrestic cache --cleanup"),
				"workingDir: /data", "workingDir: "+tt.workingDir, 1)
			body = strings.Replace(body, "                set -eu\n", "                exec > >(ts '%H:%M:%S') 2>&1\n"+
				"                set -eu\n", 1)
			body = strings.Replace(body, "            command:\n", "            env:\n              TZ: America/Chicago\n"+
				"              RESTIC_HOST:\n                fieldRef:\n                  fieldPath: metadata.namespace\n"+
				"            command:\n", 1)
			dir := writeMigrationDir(t, body)
			var buf bytes.Buffer
			if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
				t.Fatalf("migrate: %v\n%s", err, buf.String())
			}
			_, sb := migrated(t, buf.String())
			tt.check(t, sb)

			generated := filepath.Join(t.TempDir(), "generated.yaml")
			if err := os.WriteFile(generated, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := runParity(&out, generated, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
				t.Fatalf("parity: %v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), verdictEquivalent) {
				t.Errorf("want EQUIVALENT:\n%s", out.String())
			}
		})
	}
}

func TestMigrateKeepsLinesTheShellWouldRewriteVerbatim(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, line string }{
		{"a backslash-escaped glob", `restic backup --tag=mirror /mirror --exclude=\*.log`},
		{"a variable in double quotes", `restic backup --tag=mirror /mirror --exclude="$HOME/cache"`},
		{"an unquoted glob", `restic backup --tag=mirror /mirror --exclude=*.log`},
		{"a command substitution", "restic backup --tag=mirror /mirror --exclude=`cat list`"},
		{"a redirection", `restic backup --tag=mirror /mirror > /dev/null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Replace(homeClusterRelease, "restic backup --tag=mirror /mirror", tt.line, 1)
			dir := writeHomeClusterRelease(t, body)
			var buf bytes.Buffer
			if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			_, sb := migrated(t, buf.String())
			if sb.Spec.Sources != nil || !strings.Contains(sb.Spec.Script, tt.line) {
				t.Errorf("a line the shell rewrites became sources: %+v", sb.Spec)
			}
			generated := filepath.Join(t.TempDir(), "generated.yaml")
			if err := os.WriteFile(generated, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := runParity(&out, generated, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
				t.Errorf("parity of a verbatim script: %v\n%s", err, out.String())
			}
		})
	}
	if shellSpecial(`restic backup --tag=files /data --exclude='**/temp*' --exclude="app/x"`) {
		t.Error("quoted patterns were taken for shell expansion")
	}
}

func TestMigrateCarriesSecurityContextsAndParityChecksReadAccess(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "            workingDir: /\n", `            workingDir: /
            securityContext:
              runAsUser: 1000
`, 1)
	dir := writeHomeClusterRelease(t, body)
	var buf bytes.Buffer
	if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, buf.String())
	if sc := sb.Spec.ContainerSecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("containerSecurityContext = %+v, want the HelmRelease's", sc)
	}

	generated := filepath.Join(t.TempDir(), "generated.yaml")
	dropped := strings.Replace(buf.String(), "spec:\n  affinity:", "spec:\n  affinity:", 1)
	dropped = strings.Replace(dropped, "  containerSecurityContext:\n    runAsUser: 1000\n",
		"  containerSecurityContext:\n    capabilities:\n      drop: [ALL]\n", 1)
	if err := os.WriteFile(generated, []byte(dropped), 0o600); err != nil {
		t.Fatal(err)
	}
	original := writeHomeClusterRelease(t, homeClusterRelease)
	var out bytes.Buffer
	err := runParity(&out, generated, original, migrateOptions{repository: sopsRepoURL})
	if !errors.Is(err, ErrParityDiffers) || !strings.Contains(out.String(), "READ ACCESS DIFFERS") {
		t.Errorf("a backup that can no longer read every file passed parity: %v\n%s", err, out.String())
	}
}

func TestMigrateReadsTheScriptFromArgs(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "            command:\n              - sh\n              - -c\n              - |\n",
		"            command: [sh, -c]\n            args:\n              - |\n", 1)
	dir := writeHomeClusterRelease(t, body)
	var buf bytes.Buffer
	if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, buf.String())
	if sb.Spec.Script != "" || len(sb.Spec.Sources) != 2 || sb.Spec.Sources[1].Paths[0] != mirrorPath {
		t.Errorf("script %q, sources %+v; want the script in args read as sources", sb.Spec.Script, sb.Spec.Sources)
	}
}

func TestMigrateRefusesACommandItCannotRead(t *testing.T) {
	t.Parallel()
	for name, command := range map[string]string{
		"extra arguments after the script": "            command: [sh, -c]\n            args:\n              - restic backup /mirror\n              - argv0\n",
		"another shell":                    "            command: [bash, -c]\n            args:\n              - restic backup /mirror\n",
		"the image's entrypoint":           "            args: [backup, /mirror]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			before, rest, _ := strings.Cut(homeClusterRelease, "            command:\n")
			_, after, _ := strings.Cut(rest, "            resources:\n")
			body := before + command + "            resources:\n" + after
			err := runMigrate(&bytes.Buffer{}, writeHomeClusterRelease(t, body), migrateOptions{repository: sopsRepoURL})
			if !errors.Is(err, ErrUnsupportedCommand) {
				t.Errorf("err = %v, want ErrUnsupportedCommand", err)
			}
		})
	}
}

func TestMigrateRefusesWhatItWouldDrop(t *testing.T) {
	t.Parallel()
	const restic = "      restic:\n        type: cronjob\n"
	tests := []struct{ name, old, new string }{
		{"an init container", restic, restic + "        initContainers:\n          prepare:\n" +
			"            image: {repository: busybox, tag: '1'}\n            command: [sh, -c, 'true']\n"},
		{"a second container", "        containers:\n", "        containers:\n          sidecar:\n" +
			"            image: {repository: busybox, tag: '1'}\n            args: [sleep, '60']\n"},
		{"another controller", "      init:\n", "      notify:\n        type: job\n        containers:\n" +
			"          notify:\n            command: [sh, -c, 'curl https://example.com']\n      init:\n"},
		{"an init Job that does more than restic init", `command: [sh, -c, "restic init || true"]`,
			`command: [sh, -c, "restic init || true; restic unlock"]`},
		{"a disabled backup", restic, restic + "        enabled: false\n"},
		{"a restic controller that is not a CronJob", restic, "      restic:\n        type: deployment\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(homeClusterRelease, tt.old) {
				t.Fatalf("fixture has no %q", tt.old)
			}
			body := strings.Replace(homeClusterRelease, tt.old, tt.new, 1)
			out, err := migrate(t, body)
			if !errors.Is(err, ErrUnsupportedController) {
				t.Errorf("err = %v, want ErrUnsupportedController\n%s", err, out)
			}
		})
	}
}

func TestMigrateNotesTheResticInitJobItReplaces(t *testing.T) {
	t.Parallel()
	out, err := migrate(t, homeClusterRelease)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(out, "# init: ") {
		t.Errorf("the dropped restic init Job was not called out:\n%s", out)
	}
}

func TestMigrateTakesOnlyResticsCacheForTheCache(t *testing.T) {
	t.Parallel()
	t.Run("a claim named cache that the script backs up", func(t *testing.T) {
		t.Parallel()
		body := strings.Replace(homeClusterRelease, "restic backup --tag=mirror /mirror", "restic backup --tag=mirror /cache", 1)
		body += "      cache:\n        existingClaim: app-cache\n"
		out, err := migrate(t, body)
		if !errors.Is(err, ErrUnsupportedMount) {
			t.Errorf("err = %v, want the claim at /cache refused, since the operator mounts restic's cache there\n%s", err, out)
		}
	})
	t.Run("a claim named cache mounted elsewhere", func(t *testing.T) {
		t.Parallel()
		body := strings.Replace(homeClusterRelease, "restic backup --tag=mirror /mirror", "restic backup --tag=mirror /appcache", 1)
		body += "      cache:\n        existingClaim: app-cache\n        globalMounts:\n          - path: /appcache\n"
		out, err := migrate(t, body)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		_, sb := migrated(t, out)
		if !slices.ContainsFunc(sb.Spec.Volumes, func(v b2backupv1.VolumeSpec) bool {
			return v.ExistingClaim == "app-cache" && v.EffectiveMountPath() == "/appcache"
		}) || sb.Spec.Cache != nil || sb.Spec.Script != "" {
			t.Errorf("volumes %+v, cache %+v, script %q; want app-cache backed up as data", sb.Spec.Volumes,
				sb.Spec.Cache, sb.Spec.Script)
		}
	})
	t.Run("restic's cache under another name", func(t *testing.T) {
		t.Parallel()
		out, err := migrate(t, homeClusterRelease+"      restic-cache:\n        type: emptyDir\n        globalMounts:\n          - path: /cache\n")
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		_, sb := migrated(t, out)
		if sb.Spec.Scratch != nil {
			t.Errorf("scratch = %+v; restic's cache at /cache became scratch space", sb.Spec.Scratch)
		}
	})
	t.Run("a cache RESTIC_CACHE_DIR points at", func(t *testing.T) {
		t.Parallel()
		body := strings.Replace(homeClusterRelease, "              TZ: America/Chicago\n",
			"              TZ: America/Chicago\n              RESTIC_CACHE_DIR: /restic-cache\n", 1)
		out, err := migrate(t, body+"      cache:\n        existingClaim: app-cache\n        globalMounts:\n          - path: /restic-cache\n")
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		_, sb := migrated(t, out)
		if len(sb.Spec.Volumes) != 1 || sb.Spec.Env[envResticCacheDir] != "" {
			t.Errorf("volumes %+v, env %v; want the cache claim taken as the cache and RESTIC_CACHE_DIR left "+
				"to the image's /cache, where the operator mounts its own", sb.Spec.Volumes, sb.Spec.Env)
		}
	})
}

func TestMigrateAppliesGlobalAndAdvancedMountsTogether(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "      mirror:\n        existingClaim: app-mirror\n",
		"      mirror:\n        existingClaim: app-mirror\n        globalMounts:\n          - path: /everywhere\n", 1)
	out, err := migrate(t, body)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, out)
	paths := make([]string, 0, len(sb.Spec.Volumes))
	for _, v := range sb.Spec.Volumes {
		paths = append(paths, v.ExistingClaim+":"+v.EffectiveMountPath())
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"app-mirror:/everywhere", "app-mirror:" + mirrorPath}) {
		t.Errorf("volumes = %v, want app-mirror at both /everywhere and /mirror, as app-template mounts it", paths)
	}
}

func TestMigrateRefusesSubPaths(t *testing.T) {
	t.Parallel()
	for name, mount := range map[string]string{
		"subPath on scratch space": "      scratch:\n        type: emptyDir\n        globalMounts:\n" +
			"          - path: /backup\n            subPath: nested\n",
		"subPathExpr on a claim": "      more:\n        existingClaim: app-more\n        globalMounts:\n" +
			"          - path: /more\n            subPathExpr: $(POD_NAME)\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := migrate(t, homeClusterRelease+mount)
			if !errors.Is(err, ErrUnsupportedMount) {
				t.Errorf("err = %v, want ErrUnsupportedMount\n%s", err, out)
			}
		})
	}
}

func TestMigrateAMariaDBBackupTheOperatorAccepts(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "restic backup --tag=db --stdin-from-command -- backup-cnpg.sh",
		"restic backup --tag=db --stdin-from-command -- dumpdb mariadb", 1)
	body = strings.Replace(body, "              TZ: America/Chicago\n", "              TZ: America/Chicago\n"+
		"              DB_HOST: mariadb.db.svc\n              DB_DATABASE: app\n              DB_USERNAME: app\n", 1)
	body = strings.Replace(body, "      postgresql-app:\n        type: secret\n        name: postgresql-app\n",
		"      mariadb:\n        type: secret\n        name: mariadb\n", 1)
	dir := writeHomeClusterRelease(t, body)
	var buf bytes.Buffer
	if err := runMigrate(&buf, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, buf.String())
	if db := sb.Spec.Database; db == nil || db.Host != "mariadb.db.svc" || db.Name != "app" || db.User != "app" {
		t.Errorf("database = %+v, want the connection from DB_HOST, DB_DATABASE and DB_USERNAME", db)
	}
	for _, name := range []string{"DB_HOST", "DB_DATABASE", "DB_USERNAME"} {
		if _, ok := sb.Spec.Env[name]; ok {
			t.Errorf("spec.env sets %s, which the operator sets from spec.database and refuses in spec.env", name)
		}
	}

	generated := filepath.Join(t.TempDir(), "generated.yaml")
	if err := os.WriteFile(generated, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runParity(&out, generated, dir, migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Errorf("parity: %v\n%s", err, out.String())
	}
}

func migrateAndParity(t *testing.T, body string, opts migrateOptions) (*b2backupv1.ScheduledBackup, string, string, error) {
	t.Helper()
	dir := writeHomeClusterRelease(t, body)
	var buf bytes.Buffer
	if err := runMigrate(&buf, dir, opts); err != nil {
		t.Fatalf("migrate: %v\n%s", err, buf.String())
	}
	_, sb := migrated(t, buf.String())
	generated := filepath.Join(t.TempDir(), "generated.yaml")
	if err := os.WriteFile(generated, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runParity(&out, generated, dir, opts)
	return sb, buf.String(), out.String(), err
}

func TestMigrateCarriesWhatTheCRDHolds(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "    defaultPodOptions:\n", `    defaultPodOptions:
      nodeSelector: {kubernetes.io/arch: amd64}
      tolerations:
        - {key: dedicated, operator: Equal, value: backup, effect: NoSchedule}
      labels: {team: storage}
`, 1)
	body = strings.Replace(body, "        type: cronjob\n", `        type: cronjob
        pod:
          priorityClassName: low
          nodeSelector: {kubernetes.io/hostname: alpha}
`, 1)
	body = strings.Replace(body, "          concurrencyPolicy: Forbid\n",
		"          concurrencyPolicy: Forbid\n          suspend: true\n          successfulJobsHistory: 5\n", 1)
	body = strings.Replace(body, "              requests:\n                cpu: 10m\n                memory: 256Mi\n", "", 1)
	body = strings.Replace(body, "            resources:\n", "", 1)

	sb, migrateOut, parityOut, err := migrateAndParity(t, body, migrateOptions{repository: sopsRepoURL})
	s := sb.Spec
	if !s.Suspend || s.NodeSelector["kubernetes.io/hostname"] != "alpha" || len(s.NodeSelector) != 1 ||
		len(s.Tolerations) != 1 || s.PriorityClassName != "low" || s.PodLabels["team"] != "storage" ||
		ptr.Deref(s.SuccessfulJobsHistoryLimit, 0) != 5 {
		t.Errorf("spec = %+v; want suspend, the pod's nodeSelector over the default, tolerations, "+
			"priorityClassName, labels and the history limit carried over", s)
	}
	if s.Resources == nil || len(s.Resources.Limits) != 0 || len(s.Resources.Requests) != 0 {
		t.Errorf("resources = %+v, want empty, as the original sets none, not the operator's limits", s.Resources)
	}
	if s.Image != "" || !strings.Contains(migrateOut, "image: ghcr.io/gabe565/restic") {
		t.Errorf("image = %q; want the operator's image, a fork of the upstream one, and the switch noted:\n%s",
			s.Image, migrateOut)
	}
	if !errors.Is(err, ErrParityDiffers) || strings.Contains(parityOut, "DIFFER") ||
		!strings.Contains(parityOut, "DUMP FORMAT CHANGES") {
		t.Errorf("parity = %v, want only the dump change:\n%s\n%s", err, parityOut, migrateOut)
	}
}

func TestMigrateCarriesAnotherImage(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "              repository: ghcr.io/gabe565/restic\n              tag: 0.19.1\n",
		"              repository: registry.example.com/backup\n              tag: \"2\"\n", 1)
	sb, _, parityOut, _ := migrateAndParity(t, body, migrateOptions{repository: sopsRepoURL})
	if sb.Spec.Image != "registry.example.com/backup:2" || strings.Contains(parityOut, "IMAGE DIFFERS") {
		t.Errorf("image = %q, want the HelmRelease's kept:\n%s", sb.Spec.Image, parityOut)
	}
}

func TestMigrateRefusesWhatTheCRDCannotHold(t *testing.T) {
	t.Parallel()
	const container = "            workingDir: /\n"
	const cronjob = "          concurrencyPolicy: Forbid\n"
	tests := []struct{ name, old, new string }{
		{"a key migrate does not read", container, container + "            ports: [{containerPort: 80}]\n"},
		{"pod annotations", "    defaultPodOptions:\n", "    defaultPodOptions:\n      annotations: {a: b}\n"},
		{"labels on the CronJob", "        type: cronjob\n", "        type: cronjob\n        labels: {a: b}\n"},
		{"a retry limit", cronjob, cronjob + "          backoffLimit: 2\n"},
		{"a time limit", cronjob, cronjob + "          activeDeadlineSeconds: 600\n"},
		{"another Job lifetime", cronjob, cronjob + "          ttlSecondsAfterFinished: 60\n"},
		{"another envFrom Secret", envsSecretLine,
			"                  name: restic-envs\n              - secretRef:\n                  name: app-extra\n"},
		{"a ConfigMap in envFrom", envsSecretLine,
			"                  name: restic-envs\n              - configMapRef:\n                  name: settings\n"},
		{"a prefixed restic-envs", envsSecretLine, "                  name: restic-envs\n                prefix: X_\n"},
		{"no restic-envs", "              - secretRef:\n                  name: restic-envs\n", ""},
		{"values from elsewhere", specValuesLines, "spec:\n  valuesFrom:\n    - kind: ConfigMap\n      name: extra\n  values:\n"},
		{"a post-renderer", specValuesLines, "spec:\n  postRenderers:\n    - kustomize: {patches: []}\n  values:\n"},
		{"another chart", specValuesLines, "spec:\n  chart:\n    spec:\n      chart: other\n  values:\n"},
		{"a Helm template", "workingDir: /\n", "workingDir: \"{{ .Release.Namespace }}\"\n"},
		{"a defaultPodOptionsStrategy", "    defaultPodOptions:\n", "    defaultPodOptionsStrategy: merge\n    defaultPodOptions:\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(homeClusterRelease, tt.old) {
				t.Fatalf("fixture has no %q", tt.old)
			}
			out, err := migrate(t, strings.Replace(homeClusterRelease, tt.old, tt.new, 1))
			if !errors.Is(err, ErrUnsupportedValues) {
				t.Errorf("err = %v, want ErrUnsupportedValues\n%s", err, out)
			}
		})
	}
}

func TestMigrateWarnsItCannotSeeInsideResticEnvs(t *testing.T) {
	t.Parallel()
	out, err := migrate(t, homeClusterRelease)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(out, "# restic-envs: ") || !strings.Contains(out, "--from-secret") {
		t.Errorf("no warning that restic-envs' other keys went unchecked:\n%s", out)
	}
}

func TestMigrateCarriesResticEnvsKeysFromTheSecret(t *testing.T) {
	secret := fakeSops(t, sopsEmitting("metadata:\n  namespace: "+kustomizedNS+"\nstringData:\n  RESTIC_REPOSITORY: "+
		sopsRepoURL+"\n  RESTIC_PASSWORD: hunter2\n  RESTIC_COMPRESSION: max\n  RESTIC_READ_CONCURRENCY: \"4\"\n"))
	var buf bytes.Buffer
	if err := runMigrate(&buf, writeHomeClusterRelease(t, homeClusterRelease), migrateOptions{fromSecret: secret}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, buf.String())
	if sb.Spec.Env["RESTIC_COMPRESSION"] != "max" || sb.Spec.Env["RESTIC_READ_CONCURRENCY"] != "4" {
		t.Errorf("env = %v, want restic-envs' other RESTIC_ keys carried over", sb.Spec.Env)
	}
	if strings.Contains(buf.String(), "hunter2") || !strings.Contains(buf.String(), "RESTIC_COMPRESSION") {
		t.Errorf("want the carried keys named and the password left out:\n%s", buf.String())
	}

	for name, extra := range map[string]string{
		"a password file":      "  RESTIC_PASSWORD_FILE: /pw\n",
		"a credential":         "  RESTIC_REST_PASSWORD: x\n",
		"a key restic ignores": "  APP_TOKEN: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			secret := fakeSops(t, sopsEmitting("stringData:\n  RESTIC_REPOSITORY: "+sopsRepoURL+"\n  RESTIC_PASSWORD: p\n"+extra))
			err := runMigrate(&bytes.Buffer{}, writeHomeClusterRelease(t, homeClusterRelease), migrateOptions{fromSecret: secret})
			if !errors.Is(err, ErrUnsupportedValues) {
				t.Errorf("err = %v, want ErrUnsupportedValues", err)
			}
		})
	}
}

func TestMigrateKeepsACronJobWithoutATimeZoneOnUTC(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "          timeZone: America/Chicago\n", "", 1)
	body = strings.Replace(body, "              TZ: America/Chicago\n", "              TZ: UTC\n", 1)
	sb, migrateOut, parityOut, err := migrateAndParity(t, body, migrateOptions{repository: sopsRepoURL})
	if sb.Spec.TimeZone != "UTC" || !strings.Contains(migrateOut, "# timeZone: ") {
		t.Errorf("timeZone = %q, want UTC, the zone the original ran in, and the choice noted:\n%s",
			sb.Spec.TimeZone, migrateOut)
	}
	if strings.Contains(parityOut, "TIMEZONE DIFFERS") || !strings.Contains(parityOut, "daily at 00:32 UTC") {
		t.Errorf("parity = %v, want the original read as running in UTC:\n%s", err, parityOut)
	}
}

func writeFluxTree(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "myapp", "resources", "restic")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, "apps", "myapp", "ks.yaml"): `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: myapp
  namespace: flux-system
spec:
  targetNamespace: flux-target
  path: ./apps/myapp/resources
`,
		filepath.Join(dir, "kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: " +
			kustomizedNS + "\n",
		filepath.Join(dir, "helmrelease.yaml"): body,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestMigrateHonorsTheFluxTargetNamespace(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := runMigrate(&buf, writeFluxTree(t, homeClusterRelease), migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo, sb := migrated(t, buf.String())
	if repo.Namespace != "flux-target" || sb.Namespace != "flux-target" {
		t.Errorf("namespaces %q and %q, want the Flux Kustomization's targetNamespace", repo.Namespace, sb.Namespace)
	}

	buf.Reset()
	body := strings.Replace(homeClusterRelease, specValuesLines, "spec:\n  targetNamespace: release-target\n  values:\n", 1)
	if err := runMigrate(&buf, writeFluxTree(t, body), migrateOptions{repository: sopsRepoURL}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, sb := migrated(t, buf.String()); sb.Namespace != "release-target" {
		t.Errorf("namespace %q, want the HelmRelease's own targetNamespace, where Helm installs the CronJob", sb.Namespace)
	}
}

func TestMigrateRefusesANumberAppTemplateWouldRewrite(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "              TZ: America/Chicago\n",
		"              TZ: America/Chicago\n              RESTIC_READ_CONCURRENCY: 12\n", 1)
	out, err := migrate(t, body)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, sb := migrated(t, out); sb.Spec.Env["RESTIC_READ_CONCURRENCY"] != "12" {
		t.Errorf("env = %v, want 12 kept", sb.Spec.Env)
	}

	body = strings.Replace(body, "RESTIC_READ_CONCURRENCY: 12", "RESTIC_PACK_SIZE: 1000000", 1)
	if out, err := migrate(t, body); !errors.Is(err, ErrUnsupportedValues) || !strings.Contains(fmt.Sprint(err), "1e+06") {
		t.Errorf("err = %v, want 1000000 refused, since app-template renders it as 1e+06\n%s", err, out)
	}
}

func TestMigrateRefusesAReleaseWhoseRepositoryLivesInAnotherSecret(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, envsSecretLine, "                  name: obsidibot-envs\n", 1)
	out, err := migrate(t, body)
	if !errors.Is(err, ErrUnsupportedValues) {
		t.Errorf("err = %v; every Repository migrate writes is named restic and reads restic-envs, so a release "+
			"reading its repository from another Secret would collapse into another release's Repository\n%s", err, out)
	}
}

func TestMigrateRefusesASecretTheBackupWouldNotFind(t *testing.T) {
	t.Parallel()
	const dump = "restic backup --tag=db --stdin-from-command -- backup-cnpg.sh\n"
	tests := map[string]string{
		"a verbatim dumpdb reading its default mount": "echo start\n                restic backup --tag=db --stdin-from-command -- dumpdb cnpg\n",
		"a verbatim dumpdb told the old mount": "echo start\n                restic backup --tag=db --stdin-from-command -- " +
			"dumpdb cnpg --secret-mount=/postgresql-app\n",
		"a verbatim backup-mariadb.sh": "echo start\n                restic backup --tag=db --stdin-from-command -- backup-mariadb.sh\n",
		"a Secret no dump reads":       "echo start\n",
	}
	for name, replacement := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := migrate(t, strings.Replace(homeClusterRelease, dump, replacement, 1))
			if !errors.Is(err, ErrUnsupportedMount) {
				t.Errorf("err = %v, want ErrUnsupportedMount\n%s", err, out)
			}
		})
	}
}

func TestMigrateRefusesAnAmbiguousEnvFromItem(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "              - secretRef:\n                  name: restic-envs\n",
		"              - secret: other-secret\n                secretRef:\n                  name: restic-envs\n", 1)
	out, err := migrate(t, body)
	if !errors.Is(err, ErrUnsupportedValues) {
		t.Errorf("err = %v; app-template reads other-secret here, not restic-envs\n%s", err, out)
	}
}

func TestMigrateRefusesAnotherKeyForTheRepository(t *testing.T) {
	body := strings.Replace(homeClusterRelease, "              TZ: America/Chicago\n",
		"              TZ: America/Chicago\n              RESTIC_PASSWORD_COMMAND: cat /pw\n", 1)
	if out, err := migrate(t, body); !errors.Is(err, ErrUnsupportedValues) {
		t.Errorf("err = %v, want RESTIC_PASSWORD_COMMAND in env refused\n%s", err, out)
	}

	secret := fakeSops(t, sopsEmitting("stringData:\n  RESTIC_REPOSITORY: "+sopsRepoURL+"\n"))
	err := runMigrate(&bytes.Buffer{}, writeHomeClusterRelease(t, homeClusterRelease), migrateOptions{fromSecret: secret})
	if !errors.Is(err, ErrUnsupportedValues) {
		t.Errorf("err = %v, want a restic-envs without RESTIC_PASSWORD refused, since the Repository reads it", err)
	}
}

func TestMigrateLetsAnEmptyPodMapClearTheDefault(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "    defaultPodOptions:\n",
		"    defaultPodOptions:\n      nodeSelector: {disk: ssd}\n      labels: {team: storage}\n", 1)
	body = strings.Replace(body, "        type: cronjob\n", "        type: cronjob\n        pod:\n          nodeSelector: {}\n          labels: {}\n", 1)
	sb, _, parityOut, _ := migrateAndParity(t, body, migrateOptions{repository: sopsRepoURL})
	if len(sb.Spec.NodeSelector) != 0 || len(sb.Spec.PodLabels) != 0 {
		t.Errorf("nodeSelector %v, podLabels %v; app-template renders neither when the pod sets them empty",
			sb.Spec.NodeSelector, sb.Spec.PodLabels)
	}
	if strings.Contains(parityOut, "NODE SELECTOR DIFFERS") || strings.Contains(parityOut, "POD LABELS DIFFER") {
		t.Errorf("parity disagrees:\n%s", parityOut)
	}
}

func TestParityTakesTheImagesZoneForAMissingTZ(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "          timeZone: America/Chicago\n", "", 1)
	body = strings.Replace(body, "              TZ: America/Chicago\n", "", 1)
	_, _, parityOut, _ := migrateAndParity(t, body, migrateOptions{repository: sopsRepoURL})
	if strings.Contains(parityOut, "ENV DIFFERS") {
		t.Errorf("the image sets no TZ, so the original ran in UTC as the generated backup does:\n%s", parityOut)
	}
}

func TestMigrateDoesNotTakeACacheDirFlagForData(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "restic backup --tag=mirror /mirror", "restic backup --tag=mirror --cache-dir=/cache /mirror", 1)
	out, err := migrate(t, body+"      restic-cache:\n        type: emptyDir\n        globalMounts:\n          - path: /cache\n")
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if _, sb := migrated(t, out); sb.Spec.Scratch != nil {
		t.Errorf("scratch = %+v; the emptyDir --cache-dir names is restic's cache", sb.Spec.Scratch)
	}
}

func TestMigrateTakesACacheDirTheScriptBacksUpRelativelyForData(t *testing.T) {
	t.Parallel()
	const script = "restic backup --tag=db --stdin-from-command -- backup-cnpg.sh\n                restic backup --tag=mirror /mirror\n"
	body := strings.Replace(homeClusterRelease, script, "restic backup --tag=files .\n", 1)
	body = strings.Replace(body, "      postgresql-app:\n        type: secret\n        name: postgresql-app\n", "", 1)

	srv := strings.Replace(body, "            workingDir: /\n", "            workingDir: /srv\n", 1)
	srv = strings.Replace(srv, "              TZ: America/Chicago\n", "              TZ: America/Chicago\n              RESTIC_CACHE_DIR: /srv\n", 1)
	srv = strings.Replace(srv, "              - path: /mirror\n", "              - path: /srv\n", 1)
	sb, _, parityOut, _ := migrateAndParity(t, srv, migrateOptions{repository: sopsRepoURL})
	if len(sb.Spec.Volumes) != 1 || sb.Spec.Volumes[0].EffectiveMountPath() != "/srv" {
		t.Errorf("volumes = %+v, want app-mirror at /srv, which restic backup . reads from /srv", sb.Spec.Volumes)
	}
	if strings.Contains(parityOut, volumeDiffers) {
		t.Errorf("parity:\n%s", parityOut)
	}

	cache := strings.Replace(body, "            workingDir: /\n", "            workingDir: /cache\n", 1)
	cache = strings.Replace(cache, "              - path: /mirror\n", "              - path: /cache\n", 1)
	if out, err := migrate(t, cache); !errors.Is(err, ErrUnsupportedMount) {
		t.Errorf("err = %v, want app-mirror at /cache, backed up as ., refused\n%s", err, out)
	}
}

func TestMigrateKeepsTwoForgetsVerbatim(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease,
		"restic forget --prune --keep-daily=90 --keep-weekly=24 --keep-monthly=24 --keep-yearly=20\n",
		"restic forget --prune --keep-daily=90\n                restic forget --prune --keep-weekly=24\n", 1)
	out, err := migrate(t, body)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, sb := migrated(t, out)
	if sb.Spec.Retention != nil || !strings.Contains(sb.Spec.Script, "--keep-daily=90") ||
		!strings.Contains(sb.Spec.Script, "--keep-weekly=24") {
		t.Errorf("retention %+v, script %q; want both forgets kept, not one dropped", sb.Spec.Retention, sb.Spec.Script)
	}
}

func TestMigrateNotesTheShellOptionsTheOperatorAdds(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "                exec > >(ts '%H:%M:%S') 2>&1\n                set -eu\n", "", 1)
	_, migrateOut, parityOut, err := migrateAndParity(t, body, migrateOptions{repository: sopsRepoURL})
	if !strings.Contains(migrateOut, "# shell: ") {
		t.Errorf("no note that set -e is added:\n%s", migrateOut)
	}
	if !errors.Is(err, ErrParityDiffers) || !strings.Contains(parityOut, "SHELL OPTIONS ADDED: errexit") {
		t.Errorf("parity = %v, want errexit reported:\n%s", err, parityOut)
	}
}

func TestMigrateStripsABraceGroupOnlyWithItsClosing(t *testing.T) {
	t.Parallel()
	const old = "                exec > >(ts '%H:%M:%S') 2>&1\n                set -eu\n"
	tests := []struct{ name, open, close string }{
		{"another ts", "                set -o pipefail\n                {\n                set -eu\n", "                } 2>&1 | ts -s\n"},
		{"a closing migrate does not know", "                {\n                set -eu\n", "                } > /dev/null\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := strings.Replace(homeClusterRelease, old, tt.open, 1)
			body = strings.Replace(body, "                restic cache --cleanup\n", "                restic cache --cleanup\n"+tt.close, 1)
			body = strings.Replace(body, "restic backup --tag=mirror /mirror", "restic backup --tag=mirror /mirror || echo failed", 1)
			sb, _, parityOut, _ := migrateAndParity(t, body, migrateOptions{repository: sopsRepoURL})
			opens, closes := strings.Count(sb.Spec.Script, "{"), strings.Count(sb.Spec.Script, "}")
			if opens != closes {
				t.Errorf("script = %q; a brace lost its partner", sb.Spec.Script)
			}
			if strings.Contains(parityOut, "SCRIPT DOES NOT PARSE") {
				t.Errorf("parity:\n%s", parityOut)
			}
		})
	}
}

func TestMigrateRefusesAScriptThatDoesNotParse(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not on PATH")
	}
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "                exec > >(ts '%H:%M:%S') 2>&1\n                set -eu\n",
		"                {\n                set -eu\n", 1)
	body = strings.Replace(body, "restic backup --tag=mirror /mirror", "restic backup --tag=mirror /mirror || echo failed", 1)
	if out, err := migrate(t, body); !errors.Is(err, ErrUnsupportedCommand) {
		t.Errorf("err = %v, want a { with no closing refused\n%s", err, out)
	}
}

func TestMigrateNotesAnErrexitThatEndedWithItsGroup(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "                exec > >(ts '%H:%M:%S') 2>&1\n                set -eu\n",
		"                set -o pipefail\n                { set -eu; restic backup --tag=pre /mirror; } 2>&1 | ts\n", 1)
	out, err := migrate(t, body)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(out, "# shell: ") {
		t.Errorf("no note that the rest of the script now runs under set -e:\n%s", out)
	}
}

func TestMigrateNotesWhatTheOperatorAddsToBackupAndForget(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "restic backup --tag=mirror /mirror", "restic backup /mirror", 1)
	body = strings.Replace(body, "restic forget --prune --keep-daily=90", "restic forget --keep-daily=90", 1)
	out, err := migrate(t, body)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(out, "# prune: ") || !strings.Contains(out, "# tags: ") {
		t.Errorf("want notes that forget now prunes and that the untagged backup is now tagged files:\n%s", out)
	}
}

func TestMigrateNotesItCouldNotParseTheScriptWithoutBash(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	out, err := migrate(t, homeClusterRelease)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(out, "# syntax: ") {
		t.Errorf("no note that the script went unparsed:\n%s", out)
	}
}

func TestMigrateReadsThePruneFlagsValue(t *testing.T) {
	t.Parallel()
	for flag, wantNote := range map[string]bool{"--prune=true": false, "--prune=false": true} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			out, err := migrate(t, strings.Replace(homeClusterRelease, "restic forget --prune ", "restic forget "+flag+" ", 1))
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			_, sb := migrated(t, out)
			if sb.Spec.Retention == nil || strings.Contains(out, "# prune: ") != wantNote {
				t.Errorf("retention %+v, prune note %t; want the policy read and the note only when forget did not prune:\n%s",
					sb.Spec.Retention, !wantNote, out)
			}
		})
	}
}

func TestParityReadsPruneTrueAsPrune(t *testing.T) {
	t.Parallel()
	if normalized("restic forget --prune=true --keep-daily=7") != normalized("restic forget --prune --keep-daily=7") {
		t.Error("--prune=true and --prune compared differently")
	}
	if normalized("restic forget --prune=false --keep-daily=7") == normalized("restic forget --prune --keep-daily=7") {
		t.Error("--prune=false compared equal to --prune")
	}
}

func TestMigrateAndParityLeaveShellOptionsUncheckedWhereTheyWouldGuess(t *testing.T) {
	t.Parallel()
	body := strings.Replace(homeClusterRelease, "                exec > >(ts '%H:%M:%S') 2>&1\n                set -eu\n",
		"                if true; then set -eu; fi\n", 1)
	_, migrateOut, parityOut, _ := migrateAndParity(t, body, migrateOptions{repository: sopsRepoURL})
	if !strings.Contains(migrateOut, "does not follow") || !strings.Contains(parityOut, "SHELL OPTIONS UNCHECKED") {
		t.Errorf("want the shell options reported as unchecked:\n%s\n%s", migrateOut, parityOut)
	}
}
