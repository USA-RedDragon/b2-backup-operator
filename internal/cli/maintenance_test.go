package cli

import (
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
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

func TestUnlockRemoveAllRefusesWhileAnythingUsesTheRepository(t *testing.T) {
	t.Parallel()
	running := func(name string, owner metav1.OwnerReference, labels map[string]string) *batchv1.Job {
		return &batchv1.Job{Namespace: testNS, Name: name, Labels: labels, OwnerReferences: []metav1.OwnerReference{owner}}
	}
	tests := []struct {
		name string
		job  func(fx currentFixture) *batchv1.Job
	}{
		{"a scheduled backup", func(fx currentFixture) *batchv1.Job {
			return running("web-files-backup-1", metav1.OwnerReference{APIVersion: batchAPIVersion, Kind: cronJobKind,
				Name: fx.cj.Name, UID: fx.cj.UID, Controller: ptr.To(true)}, nil)
		}},
		{"a b2bk download", func(fx currentFixture) *batchv1.Job {
			return running("web-files-b2bk-download-x", metav1.OwnerReference{APIVersion: b2backupv1.GroupVersion.String(),
				Kind: kindScheduledBackup, Name: fx.sb.Name, UID: fx.sb.UID},
				map[string]string{labelManagedBy: runner.ManagedByValue, runner.LabelPurpose: purposeDownload})
		}},
		{"another backup's Job on the same repository", func(fx currentFixture) *batchv1.Job {
			return running("other-manual-1", metav1.OwnerReference{APIVersion: b2backupv1.GroupVersion.String(),
				Kind: kindScheduledBackup, Name: otherBackup, UID: "uid-sb-other", Controller: ptr.To(true)}, nil)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := currentBackup(t)
			other := newBackup(otherBackup, testRepoName)
			other.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: "other"}}
			c := newClient(t, fx.objects(other, tt.job(fx))...)
			cmd := newUnlockCommand(factoryWithClient(t, c))
			cmd.SetArgs([]string{subjectBackup, "--remove-all", flagYes})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "is running") {
				t.Errorf("err = %v, want --remove-all refused while a Job may hold a live lock", err)
			}
		})
	}
}

func TestUnlockRemoveAllRefusesARepositoryThatIsNotItsOwn(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t)
	fx.repo.Status.Conditions = []metav1.Condition{{
		Type: b2backupv1.RepositoryConditionReady, Status: metav1.ConditionFalse, Reason: reasonRepositoryConflict,
	}}
	cmd := newUnlockCommand(factoryWithClient(t, newClient(t, fx.objects()...)))
	cmd.SetArgs([]string{subjectBackup, "--remove-all", flagYes})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); !errors.Is(err, ErrRepositoryNotUsable) {
		t.Errorf("err = %v, want --remove-all refused while the locks may be another repository's", err)
	}
}
