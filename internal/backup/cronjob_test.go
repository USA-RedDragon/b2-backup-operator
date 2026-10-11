package backup

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
)

const (
	testNS       = "myapp-prod"
	testSchedule = "4 * * * *"
	testDBName   = "appdb"

	dbSecretMountPath = "/var/run/secrets/b2backup/database"
	testUserEnvKey    = "APP_ENV"
)

func testConfig() Config {
	return Config{
		Image:             "ghcr.io/usa-reddragon/b2-backup-operator-restic:test",
		CacheStorageClass: "nfs-regional-ssd",
	}
}

func testRepo() *b2backupv1.Repository {
	return &b2backupv1.Repository{
		Name: containerName, Namespace: testNS,
	}
}

func testBackup(mutate func(*b2backupv1.ScheduledBackup)) *b2backupv1.ScheduledBackup {
	sb := &b2backupv1.ScheduledBackup{
		Name: containerName, Namespace: testNS,
		Spec: b2backupv1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: containerName},
			Schedule:      testSchedule,
			TimeZone:      "America/Chicago",
			Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
		},
	}
	if mutate != nil {
		mutate(sb)
	}
	return sb
}

func envOf(c corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func containerOf(t *testing.T, sb *b2backupv1.ScheduledBackup, cfg Config) corev1.Container {
	t.Helper()
	cj, err := BuildCronJob(sb, testRepo(), cfg)
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	return cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
}

func TestBuildCronJobBasics(t *testing.T) {
	cj, err := BuildCronJob(testBackup(nil), testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	if cj.Spec.Schedule != testSchedule {
		t.Errorf("schedule = %q, want it passed through verbatim", cj.Spec.Schedule)
	}
	if got := ptr.Deref(cj.Spec.TimeZone, ""); got != "America/Chicago" {
		t.Errorf("timeZone = %q", got)
	}

	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if c.EnvFrom[0].SecretRef.Name != "restic-credentials" {
		t.Errorf("envFrom secret = %q", c.EnvFrom[0].SecretRef.Name)
	}

	if got := ptr.Deref(cj.Spec.JobTemplate.Spec.BackoffLimit, -1); got != 0 {
		t.Errorf("backoffLimit = %d, want 0", got)
	}
	if env := envOf(c, "RESTIC_HOST"); env == nil || env.ValueFrom == nil ||
		env.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Error("RESTIC_HOST must come from the downward API")
	}
}

func TestCacheMountsWhereTheImageExpects(t *testing.T) {
	c := containerOf(t, testBackup(nil), testConfig())
	var found bool
	for _, m := range c.VolumeMounts {
		if m.Name == cacheVolume {
			found = true
			if m.MountPath != "/cache" {
				t.Errorf("cache mounted at %q, want /cache", m.MountPath)
			}
		}
	}
	if !found {
		t.Error("no cache volume mount")
	}

	pvc, err := BuildCachePVC(testBackup(nil), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Name != containerName+"-cache" {
		t.Errorf("cache pvc name = %q", pvc.Name)
	}
	if ptr.Deref(pvc.Spec.StorageClassName, "") != "nfs-regional-ssd" {
		t.Errorf("cache storage class = %v", pvc.Spec.StorageClassName)
	}
}

func TestCommandRunsTheScriptUnderSh(t *testing.T) {
	c := containerOf(t, testBackup(nil), testConfig())

	if len(c.Command) != 3 || c.Command[0] != "sh" || c.Command[1] != "-c" {
		t.Fatalf("command = %v, want sh -c <script>", c.Command)
	}
	script, err := Render(&testBackup(nil).Spec)
	if err != nil {
		t.Fatal(err)
	}
	if c.Command[2] != script {
		t.Errorf("command script = %q, want the rendered script", c.Command[2])
	}
}

func TestMariaDBWiring(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeMariaDB}}
		s.Spec.Database = &b2backupv1.DatabaseSpec{
			Engine: b2backupv1.DatabaseEngineMariaDB,
			Host:   mariadbName, Name: testDBName, User: testDBName,
		}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	for name, want := range map[string]string{
		"DB_HOST": mariadbName, "DB_DATABASE": testDBName, "DB_USERNAME": testDBName,
	} {
		if env := envOf(c, name); env == nil || env.Value != want {
			t.Errorf("%s = %v, want %q", name, env, want)
		}
	}

	if cj.Spec.JobTemplate.Spec.Template.Labels["mariadb-client"] != "true" {
		t.Error("missing the mariadb-client pod label")
	}
}

func TestDatabaseAffinityIsAPreferenceUnlessAsked(t *testing.T) {
	mariadb := func(require bool) func(*b2backupv1.ScheduledBackup) {
		return func(s *b2backupv1.ScheduledBackup) {
			s.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeMariaDB}}
			s.Spec.Database = &b2backupv1.DatabaseSpec{
				Engine: b2backupv1.DatabaseEngineMariaDB,
				Host:   mariadbName, Name: testDBName, User: testDBName,
				RequireSameNode: require,
			}
		}
	}
	cnpg := func(require bool) func(*b2backupv1.ScheduledBackup) {
		return func(s *b2backupv1.ScheduledBackup) {
			s.Spec.Database = &b2backupv1.DatabaseSpec{
				Engine:          b2backupv1.DatabaseEngineCNPG,
				RequireSameNode: require,
			}
		}
	}

	tests := []struct {
		name         string
		mutate       func(*b2backupv1.ScheduledBackup)
		wantRequired bool
		wantLabels   map[string]string
	}{
		{
			name:       "mariadb by default",
			mutate:     mariadb(false),
			wantLabels: map[string]string{labelName: mariadbName, labelController: mariadbName},
		},
		{
			name:         "mariadb pinned to the node",
			mutate:       mariadb(true),
			wantRequired: true,
			wantLabels:   map[string]string{labelName: mariadbName, labelController: mariadbName},
		},
		{
			name:   "cnpg by default",
			mutate: cnpg(false),
			wantLabels: map[string]string{
				"cnpg.io/cluster": "postgresql", "cnpg.io/instanceRole": "primary",
			},
		},
		{
			name:         "cnpg pinned to the node",
			mutate:       cnpg(true),
			wantRequired: true,
			wantLabels: map[string]string{
				"cnpg.io/cluster": "postgresql", "cnpg.io/instanceRole": "primary",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cj, err := BuildCronJob(testBackup(tt.mutate), testRepo(), testConfig())
			if err != nil {
				t.Fatalf("BuildCronJob() error = %v", err)
			}
			aff := cj.Spec.JobTemplate.Spec.Template.Spec.Affinity
			if aff == nil || aff.PodAffinity == nil {
				t.Fatal("no pod affinity")
			}

			required := aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			preferred := aff.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution

			var sel map[string]string
			switch {
			case tt.wantRequired:
				if len(required) != 1 || len(preferred) != 0 {
					t.Fatalf("required = %v, preferred = %v, want one hard term", required, preferred)
				}
				sel = required[0].LabelSelector.MatchLabels
			default:
				if len(preferred) != 1 || len(required) != 0 {
					t.Fatalf("required = %v, preferred = %v, want one soft term; a hard "+
						"affinity to labels an app may not carry leaves the backup unschedulable",
						required, preferred)
				}
				sel = preferred[0].PodAffinityTerm.LabelSelector.MatchLabels
			}

			for k, want := range tt.wantLabels {
				if sel[k] != want {
					t.Errorf("selector[%s] = %q, want %q", k, sel[k], want)
				}
			}
		})
	}
}

func TestCNPGClusterNameIsConfigurable(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Database = &b2backupv1.DatabaseSpec{
			Engine:      b2backupv1.DatabaseEngineCNPG,
			CNPGCluster: "acme-pg",
		}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	aff := cj.Spec.JobTemplate.Spec.Template.Spec.Affinity
	sel := aff.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].
		PodAffinityTerm.LabelSelector.MatchLabels
	if sel["cnpg.io/cluster"] != "acme-pg" {
		t.Errorf("cnpg.io/cluster = %q, want the configured cluster", sel["cnpg.io/cluster"])
	}
}

func TestMariaDBWithoutConnectionDetailsIsAnError(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineMariaDB}
	})
	if _, err := BuildCronJob(sb, testRepo(), testConfig()); err == nil {
		t.Fatal("expected an error for mariadb with no host, name or user")
	}
}

func TestCNPGSetsNoDBEnv(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineCNPG}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if envOf(c, envDBHost) != nil {
		t.Error("cnpg reads its credentials from the mounted secret, not DB_* env")
	}
}

func TestVolumeAffinityWinsOverDatabase(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineCNPG}
		s.Spec.Volume = &b2backupv1.VolumeSpec{ExistingClaim: "myapp-prod-storage"}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	spec := cj.Spec.JobTemplate.Spec
	aff := spec.Template.Spec.Affinity
	if aff == nil || aff.PodAffinity == nil ||
		len(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("expected hard pod affinity to the app when a claim is attached")
	}
	sel := aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector
	if len(sel.MatchLabels) != 1 || sel.MatchLabels[labelName] != testNS {
		t.Errorf("affinity selector = %v, want only %s=%s", sel.MatchLabels, labelName, testNS)
	}
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		t.Fatal(err)
	}
	if !selector.Matches(klabels.Set{labelName: testNS, labelController: testNS}) {
		t.Error("the selector must match the app's pods whatever their controller is named")
	}
	backupPod := maps.Clone(spec.Template.Labels)
	backupPod[labelName] = testNS
	if selector.Matches(klabels.Set(backupPod)) {
		t.Error("the selector must not match a backup pod, even one named after the namespace")
	}

	c := spec.Template.Spec.Containers[0]
	if c.WorkingDir != "/myapp-prod-storage" {
		t.Errorf("workingDir = %q, want the claim mount path", c.WorkingDir)
	}
}

func TestSpecImageOverridesConfig(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) { s.Spec.Image = "custom:1.2.3" })
	if got := containerOf(t, sb, testConfig()).Image; got != "custom:1.2.3" {
		t.Errorf("image = %q", got)
	}
}

func TestUserEnvIsSorted(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Env = map[string]string{"ZED": "1", "ALPHA": "2", "MIKE": "3"}
	})
	for range 20 {
		c := containerOf(t, sb, testConfig())
		var got []string
		for _, e := range c.Env {
			if e.Name == "ZED" || e.Name == "ALPHA" || e.Name == "MIKE" {
				got = append(got, e.Name)
			}
		}
		if strings.Join(got, ",") != "ALPHA,MIKE,ZED" {
			t.Fatalf("env order = %v, want sorted", got)
		}
	}
}

func TestDatabaseSecretMountsWhereTheDumpToolIsTold(t *testing.T) {
	tests := []struct {
		name       string
		engine     b2backupv1.DatabaseEngine
		secretName string
		wantSecret string
	}{
		{"cnpg default", b2backupv1.DatabaseEngineCNPG, "", "postgresql-app"},
		{"cnpg custom secret", b2backupv1.DatabaseEngineCNPG, "myapp-db-creds", "myapp-db-creds"},
		{"mariadb default", b2backupv1.DatabaseEngineMariaDB, "", "mariadb"},
		{"mariadb custom secret", b2backupv1.DatabaseEngineMariaDB, "legacy-mysql", "legacy-mysql"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sourceType := b2backupv1.SourceTypeCNPG
			if tt.engine == b2backupv1.DatabaseEngineMariaDB {
				sourceType = b2backupv1.SourceTypeMariaDB
			}
			sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Sources = []b2backupv1.BackupSource{{Type: sourceType}}
				s.Spec.Database = &b2backupv1.DatabaseSpec{
					Engine:     tt.engine,
					SecretName: tt.secretName,
					Host:       mariadbName, Name: testDBName, User: testDBName,
				}
			})
			cj, err := BuildCronJob(sb, testRepo(), testConfig())
			if err != nil {
				t.Fatalf("BuildCronJob() error = %v", err)
			}
			spec := cj.Spec.JobTemplate.Spec.Template.Spec

			var mount *corev1.VolumeMount
			for i := range spec.Containers[0].VolumeMounts {
				if spec.Containers[0].VolumeMounts[i].Name == "db-credentials" {
					mount = &spec.Containers[0].VolumeMounts[i]
				}
			}
			if mount == nil {
				t.Fatal("no db-credentials mount")
			}

			if mount.MountPath != dbSecretMountPath {
				t.Errorf("mounted at %q, want %q", mount.MountPath, dbSecretMountPath)
			}

			script := strings.Join(spec.Containers[0].Command, " ")
			want := "--secret-mount=" + dbSecretMountPath
			if !strings.Contains(script, want) {
				t.Errorf("mounted at %q but the command does not pass %q:\n%s",
					mount.MountPath, want, script)
			}

			var volume *corev1.Volume
			for i := range spec.Volumes {
				if spec.Volumes[i].Name == "db-credentials" {
					volume = &spec.Volumes[i]
				}
			}
			if volume == nil || volume.Secret == nil {
				t.Fatal("no db-credentials volume")
			}
			if volume.Secret.SecretName != tt.wantSecret {
				t.Errorf("volume references Secret %q, want %q", volume.Secret.SecretName, tt.wantSecret)
			}
		})
	}
}

func TestBackupPodSecurityDefaults(t *testing.T) {
	cj, err := BuildCronJob(testBackup(nil), testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	spec := cj.Spec.JobTemplate.Spec.Template.Spec

	if automount := spec.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("backup pod should not mount a service account token")
	}

	pod := spec.SecurityContext
	if pod == nil || pod.SeccompProfile == nil ||
		pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext = %+v, want the RuntimeDefault seccomp profile", pod)
	}
	if pod.RunAsUser != nil || pod.RunAsGroup != nil || pod.RunAsNonRoot != nil {
		t.Errorf("pod securityContext pins an id (%+v); the backup has to read data "+
			"whose ownership varies per app", pod)
	}
	if pod.FSGroup != nil {
		t.Errorf("fsGroup = %v, want it unset so backing up cannot chown the app's data", *pod.FSGroup)
	}

	container := spec.Containers[0].SecurityContext
	if container == nil {
		t.Fatal("no container securityContext")
	}
	if container.Capabilities == nil ||
		!slices.Contains(container.Capabilities.Drop, corev1.Capability("ALL")) {
		t.Errorf("capabilities = %v, want ALL dropped", container.Capabilities)
	}
	if container.AllowPrivilegeEscalation == nil || *container.AllowPrivilegeEscalation {
		t.Error("privilege escalation should be forbidden")
	}

	if container.ReadOnlyRootFilesystem != nil && *container.ReadOnlyRootFilesystem {
		t.Error("read-only root filesystem should be opt-in, not the default")
	}
}

func TestPodSecurityContextOverride(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.PodSecurityContext = &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true),
			RunAsUser:    ptr.To(int64(1000)),
			FSGroup:      ptr.To(int64(1000)),
		}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	sc := cj.Spec.JobTemplate.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatalf("securityContext = %+v, want the override applied", sc)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("runAsUser = %v, want 1000", sc.RunAsUser)
	}
}

func TestContainerSecurityContextOverride(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.ContainerSecurityContext = &corev1.SecurityContext{
			ReadOnlyRootFilesystem: ptr.To(true),
			Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	})
	c := containerOf(t, sb, testConfig())
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("securityContext = %+v, want the override applied", c.SecurityContext)
	}
}

func TestReservedEnvNamesAreRejected(t *testing.T) {
	mariadb := func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeMariaDB}}
		s.Spec.Database = &b2backupv1.DatabaseSpec{
			Engine: b2backupv1.DatabaseEngineMariaDB,
			Host:   mariadbName, Name: testDBName, User: testDBName,
		}
	}
	cnpg := func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineCNPG}
	}

	tests := []struct {
		name    string
		setup   func(*b2backupv1.ScheduledBackup)
		key     string
		wantErr bool
	}{
		{name: "repository url", key: envResticRepository, wantErr: true},
		{name: "encryption key", key: envResticPassword, wantErr: true},
		{name: "s3 key id", key: envAccessKeyID, wantErr: true},
		{name: "s3 key", key: envSecretAccessKey, wantErr: true},
		{name: "s3 region", key: envDefaultRegion, wantErr: true},
		{name: "snapshot host", key: envResticHost, wantErr: true},
		{name: "time zone", key: envTZ, wantErr: true},
		{name: "mariadb host", setup: mariadb, key: envDBHost, wantErr: true},
		{name: "mariadb database", setup: mariadb, key: envDBDatabase, wantErr: true},
		{name: "cnpg host, which the operator never sets", setup: cnpg, key: envDBHost},
		{name: "restic group by", key: "RESTIC_GROUP_BY"},
		{name: "rest username", key: "RESTIC_REST_USERNAME"},
		{name: "rest password", key: "RESTIC_REST_PASSWORD"},
		{name: "an unrelated variable", key: testUserEnvKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
				if tt.setup != nil {
					tt.setup(s)
				}
				s.Spec.Env = map[string]string{tt.key: "attacker"}
			})

			cj, err := BuildCronJob(sb, testRepo(), testConfig())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("%s was accepted from spec.env, which silently overrides envFrom", tt.key)
				}
				if !strings.Contains(err.Error(), tt.key) {
					t.Errorf("error %q does not name %s", err, tt.key)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildCronJob() error = %v, want %s accepted", err, tt.key)
			}
			c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
			if env := envOf(c, tt.key); env == nil || env.Value != "attacker" {
				t.Errorf("%s = %v, want it passed through", tt.key, env)
			}
		})
	}
}

func TestReservedEnvErrorNamesEveryConflict(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Env = map[string]string{
			envResticRepository: "rest:https://attacker/",
			envResticPassword:   "pwn",
			testUserEnvKey:      "production",
		}
	})
	_, err := BuildCronJob(sb, testRepo(), testConfig())
	if err == nil {
		t.Fatal("expected a rejection")
	}
	for _, want := range []string{envResticRepository, envResticPassword} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), testUserEnvKey) {
		t.Errorf("error %q blames a variable the operator does not set", err)
	}
}

func TestResticHostDefaultsToTheNamespace(t *testing.T) {
	c := containerOf(t, testBackup(nil), testConfig())
	env := envOf(c, envResticHost)
	if env == nil || env.ValueFrom == nil ||
		env.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Fatalf("%s = %v, want the downward API", envResticHost, env)
	}
	if env.Value != "" {
		t.Errorf("%s has both a literal value and a fieldRef", envResticHost)
	}
}

func TestResticHostOverridePinsTheSnapshotHost(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) { s.Spec.ResticHost = "legacy-name" })
	c := containerOf(t, sb, testConfig())
	env := envOf(c, envResticHost)
	if env == nil || env.Value != "legacy-name" {
		t.Fatalf("%s = %v, want the spec value", envResticHost, env)
	}
	if env.ValueFrom != nil {
		t.Errorf("%s still reads the namespace from the downward API", envResticHost)
	}
}

func TestPodLabelsCannotOverwriteTheOperatorsOwn(t *testing.T) {
	sb := testBackup(func(s *b2backupv1.ScheduledBackup) {
		s.Spec.Database = &b2backupv1.DatabaseSpec{
			Engine: b2backupv1.DatabaseEngineMariaDB,
			Host:   mariadbName, Name: testDBName, User: testDBName,
		}
		s.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeMariaDB}}
		s.Spec.PodLabels = map[string]string{
			labelManagedBy:   "someone-else",
			labelName:        "not-this-backup",
			"mariadb-client": "false",
			"team":           "platform",
		}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	labels := cj.Spec.JobTemplate.Spec.Template.Labels

	if labels[labelManagedBy] != managedByValue {
		t.Errorf("%s = %q, want %q; it is how the operator recognises its own objects",
			labelManagedBy, labels[labelManagedBy], managedByValue)
	}
	if labels[labelName] != sb.Name {
		t.Errorf("%s = %q, want %q", labelName, labels[labelName], sb.Name)
	}
	if labels["mariadb-client"] != "false" {
		t.Errorf("mariadb-client = %q, want the user value", labels["mariadb-client"])
	}
	if labels["team"] != "platform" {
		t.Errorf("team = %q, want the user label kept", labels["team"])
	}
}

func TestCacheIsOnUntilTurnedOff(t *testing.T) {
	tests := []struct {
		name  string
		cache *b2backupv1.CacheSpec
		want  bool
	}{
		{name: "no cache stanza", want: true},
		{name: "cache stanza with no enabled field", cache: &b2backupv1.CacheSpec{}, want: true},
		{name: "explicitly on", cache: &b2backupv1.CacheSpec{Enabled: ptr.To(true)}, want: true},
		{name: "explicitly off", cache: &b2backupv1.CacheSpec{Enabled: ptr.To(false)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := testBackup(func(s *b2backupv1.ScheduledBackup) { s.Spec.Cache = tt.cache })

			pvc, err := BuildCachePVC(sb, testConfig())
			if err != nil {
				t.Fatal(err)
			}
			if (pvc != nil) != tt.want {
				t.Errorf("cache pvc = %v, want built = %v", pvc, tt.want)
			}

			c := containerOf(t, sb, testConfig())
			var mounted bool
			for _, m := range c.VolumeMounts {
				if m.Name == cacheVolume {
					mounted = true
				}
			}
			if mounted != tt.want {
				t.Errorf("cache mounted = %v, want %v", mounted, tt.want)
			}
		})
	}
}

func TestManualJobNameFitsTheBudget(t *testing.T) {
	sb := testBackup(nil)
	sb.Name = strings.Repeat("a", 200)

	name := ManualJobName(sb, time.Unix(1788000000, 0))
	if len(name) > maxManualJobName {
		t.Errorf("name = %q (%d chars), want at most %d", name, len(name), maxManualJobName)
	}
	if !strings.HasSuffix(name, "-manual-tkj1mo") {
		t.Errorf("name = %q, want the trigger timestamp kept", name)
	}
}

func TestManualJobNameSuffixLongerThanTheBudget(t *testing.T) {
	suffix := "-manual-" + strings.Repeat("z", maxManualJobName)
	if got := manualJobName("app", suffix); got != suffix {
		t.Errorf("manualJobName = %q, want %q", got, suffix)
	}
}
