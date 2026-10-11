package backup

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/ptr"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func TestCronJobUsesBuildJobTemplate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*b2backupv1.ScheduledBackup)
	}{
		{name: "defaults"},
		{name: "with a volume", mutate: func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Volume = &b2backupv1.VolumeSpec{ExistingClaim: "app-data"}
			sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}
		}},
		{name: "with a database", mutate: func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Database = &b2backupv1.DatabaseSpec{
				Engine: b2backupv1.DatabaseEngineMariaDB,
				Host:   mariadbName, Name: testDBName, User: "root",
			}
			sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeMariaDB}}
		}},
		{name: "with the cache off", mutate: func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Cache = &b2backupv1.CacheSpec{Enabled: ptr.To(false)}
		}},
		{name: "with extra env", mutate: func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Env = map[string]string{testUserEnvKey: "production"}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb, repo, cfg := testBackup(tc.mutate), testRepo(), testConfig()

			tmpl, err := BuildJobTemplate(sb, repo, cfg)
			if err != nil {
				t.Fatalf("BuildJobTemplate: %v", err)
			}
			cj, err := BuildCronJob(sb, repo, cfg)
			if err != nil {
				t.Fatalf("BuildCronJob: %v", err)
			}

			if !equality.Semantic.DeepEqual(cj.Spec.JobTemplate, tmpl) {
				t.Errorf("CronJob job template diverged from BuildJobTemplate\n cronjob: %+v\ntemplate: %+v",
					cj.Spec.JobTemplate, tmpl)
			}
		})
	}
}

func TestJobTemplateCarriesManagedByLabel(t *testing.T) {
	tmpl, err := BuildJobTemplate(testBackup(nil), testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildJobTemplate: %v", err)
	}
	if got := tmpl.Labels[labelManagedBy]; got != managedByValue {
		t.Errorf("job template label %s = %q, want %q", labelManagedBy, got, managedByValue)
	}
}

func TestBuildJobTemplateRequiresAnImage(t *testing.T) {
	cfg := testConfig()
	cfg.Image = ""
	if _, err := BuildJobTemplate(testBackup(nil), testRepo(), cfg); err == nil {
		t.Error("expected an error when no image is configured")
	}
}

func TestSuspendReachesTheCronJob(t *testing.T) {
	for _, suspend := range []bool{true, false} {
		sb := testBackup(func(s *b2backupv1.ScheduledBackup) { s.Spec.Suspend = suspend })
		cj, err := BuildCronJob(sb, testRepo(), testConfig())
		if err != nil {
			t.Fatalf("BuildCronJob: %v", err)
		}
		if cj.Spec.Suspend == nil || *cj.Spec.Suspend != suspend {
			t.Errorf("suspend = %v, want %v", cj.Spec.Suspend, suspend)
		}
	}
}
