package cli

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const keepDaily = "--keep-daily=7"

func TestPruneIsScopedToTheBackupsOwnSnapshots(t *testing.T) {
	t.Parallel()
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []b2backupv1.BackupSource{
		{Type: b2backupv1.SourceTypeCNPG},
		{Type: b2backupv1.SourceTypeFiles, Tag: serverTag},
		{Type: b2backupv1.SourceTypeFiles, Tag: serverTag, Paths: []string{"/more"}},
	}
	sb.Spec.ResticHost = testResticHost
	argv, err := pruneArgs(sb, []string{keepDaily})
	if err != nil {
		t.Fatal(err)
	}
	want := "forget --prune --host=" + testResticHost + " --group-by=tags --tag=db --tag=server " + keepDaily
	if got := strings.Join(argv, " "); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}

	script := newBackup(testBackupName, testRepoName)
	script.Spec.Script = backupData
	if _, err := pruneArgs(script, []string{keepDaily}); err == nil {
		t.Error("a script backup was pruned without any tag to scope forget to")
	}
}

func TestPruneRefusesABackupInATagConflict(t *testing.T) {
	t.Parallel()
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}}
	sb.Status.Conditions = []metav1.Condition{{
		Type: b2backupv1.ScheduledBackupConditionReady, Status: metav1.ConditionFalse, Reason: "TagConflict",
		Message: "ScheduledBackup other also backs up under \"db\"",
	}}
	if _, err := pruneArgs(sb, []string{keepDaily}); err == nil || !strings.Contains(err.Error(), "tag conflict") {
		t.Errorf("err = %v, want prune refused", err)
	}
}
