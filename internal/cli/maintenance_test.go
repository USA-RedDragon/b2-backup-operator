package cli

import (
	"strings"
	"testing"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func TestPruneIsScopedToTheBackupsOwnSnapshots(t *testing.T) {
	t.Parallel()
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []b2backupv1.BackupSource{
		{Type: b2backupv1.SourceTypeCNPG},
		{Type: b2backupv1.SourceTypeFiles, Tag: serverTag},
		{Type: b2backupv1.SourceTypeFiles, Tag: serverTag, Paths: []string{"/more"}},
	}
	sb.Spec.ResticHost = testResticHost
	argv, err := pruneArgs(sb, []string{"--keep-daily=7"})
	if err != nil {
		t.Fatal(err)
	}
	want := "forget --prune --host=" + testResticHost + " --group-by=tags --tag=db --tag=server --keep-daily=7"
	if got := strings.Join(argv, " "); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}

	script := newBackup(testBackupName, testRepoName)
	script.Spec.Script = backupData
	if _, err := pruneArgs(script, []string{"--keep-daily=7"}); err == nil {
		t.Error("a script backup was pruned without any tag to scope forget to")
	}
}
