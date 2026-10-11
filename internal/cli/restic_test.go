package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/utils/ptr"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

func TestSourceTags(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []b2backupv1.BackupSource{
		{Type: b2backupv1.SourceTypeCNPG},
		{Type: b2backupv1.SourceTypeFiles},

		{Type: b2backupv1.SourceTypeCNPG, Tag: testDBTag},

		{Type: b2backupv1.SourceTypeFiles},
	}

	got := sourceTags(sb)
	want := []string{"db", "files", testDBTag}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sourceTags = %v, want %v", got, want)
	}
}

func TestRetentionFlags(t *testing.T) {
	if got := retentionFlags(nil); got != nil {
		t.Errorf("retentionFlags(nil) = %v, want nil", got)
	}
	if got := retentionFlags(&b2backupv1.Retention{}); got != nil {
		t.Errorf("an empty policy should produce no flags, got %v", got)
	}

	got := retentionFlags(&b2backupv1.Retention{
		Hourly:  ptr.To(int32(168)),
		Daily:   ptr.To(int32(90)),
		Monthly: ptr.To(int32(24)),
		Yearly:  ptr.To(int32(10)),
	})
	want := "--keep-hourly=168 --keep-daily=90 --keep-monthly=24 --keep-yearly=10"
	if strings.Join(got, " ") != want {
		t.Errorf("retentionFlags =\n %v\nwant\n %v", strings.Join(got, " "), want)
	}
}

func TestResolveRunTargetFindsABackupForARepository(t *testing.T) {
	repo := newRepo(testNS)
	sb := newBackup(testBackupName, testRepoName)
	c := newClient(t, repo, sb)

	gotSB, gotRepo, err := resolveRunTarget(context.Background(), c, testNS, "repo/"+testRepoName)
	if err != nil {
		t.Fatalf("resolveRunTarget: %v", err)
	}
	if gotSB.Name != testBackupName {
		t.Errorf("backup = %q, want %q", gotSB.Name, testBackupName)
	}
	if gotRepo.Name != testRepoName {
		t.Errorf("repository = %q, want %q", gotRepo.Name, testRepoName)
	}
}

func TestResolveRunTargetWithoutAnyBackup(t *testing.T) {
	c := newClient(t, newRepo(testNS))

	_, _, err := resolveRunTarget(context.Background(), c, testNS, "repo/"+testRepoName)
	if !errors.Is(err, ErrNoBackupForRepository) {
		t.Fatalf("expected ErrNoBackupForRepository, got %v", err)
	}
}

func TestResolveRunTargetIgnoresUnrelatedBackups(t *testing.T) {
	repo := newRepo(testNS)
	other := newBackup("other", "somewhere-else")
	c := newClient(t, repo, other)

	if _, _, err := resolveRunTarget(context.Background(), c, testNS, "repo/"+testRepoName); !errors.Is(err, ErrNoBackupForRepository) {
		t.Fatalf("expected ErrNoBackupForRepository, got %v", err)
	}
}

func TestResticCommand(t *testing.T) {
	got := resticCommand("snapshots", "--tag=files")
	if strings.Join(got, " ") != "restic snapshots --tag=files" {
		t.Errorf("resticCommand = %v", got)
	}
}

func TestSnapshotTagArgs(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []b2backupv1.BackupSource{
		{Type: b2backupv1.SourceTypeFiles},
		{Type: b2backupv1.SourceTypeCNPG, Tag: testDBTag},
	}
	got, err := snapshotTagArgs(sb)
	if err != nil {
		t.Fatalf("snapshotTagArgs: %v", err)
	}
	if want := "--tag=files --tag=" + testDBTag; strings.Join(got, " ") != want {
		t.Errorf("snapshotTagArgs = %v, want %v", got, want)
	}
}

func TestSnapshotTagArgsRefusesAScriptBackup(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Script = "restic backup /data"

	_, err := snapshotTagArgs(sb)
	if !errors.Is(err, errUnknownSourceTags) {
		t.Fatalf("expected errUnknownSourceTags, got %v", err)
	}
	if !strings.Contains(err.Error(), "--all-tags") {
		t.Errorf("the error must say how to list anyway: %v", err)
	}
}

func TestStatsRejectsAnUnknownMode(t *testing.T) {
	for _, mode := range []string{"", "bogus", "Restore-Size"} {
		cmd := newStatsCommand(NewFactory(testStreams()))
		if err := cmd.Flags().Set("mode", mode); err != nil {
			t.Fatal(err)
		}
		if err := cmd.RunE(cmd, []string{testRepoName}); !errors.Is(err, errUnknownStatsMode) {
			t.Errorf("--mode=%q: expected errUnknownStatsMode, got %v", mode, err)
		}
	}

	got := newStatsCommand(NewFactory(testStreams())).Flags().Lookup("mode").DefValue
	if !slices.Contains(statsModes, got) {
		t.Errorf("the default mode %q is not one of the accepted modes", got)
	}
}

func TestFactoryRunnerReportsProgressToStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	streams := genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: &out, ErrOut: &errOut}

	run, err := offlineFactory(t, streams).Runner()
	if err != nil {
		t.Fatalf("Runner: %v", err)
	}
	if run.Progress != &errOut {
		t.Errorf("Progress = %#v, want the stderr stream", run.Progress)
	}
	if run.Progress == &out {
		t.Error("progress must never reach stdout")
	}
}

func TestPodStartTimeoutFlagReachesEveryRunner(t *testing.T) {
	f := offlineFactory(t, testStreams())
	cmd := &cobra.Command{Use: "exec"}
	f.AddPodStartTimeoutFlag(cmd)

	run, err := f.Runner()
	if err != nil {
		t.Fatalf("Runner: %v", err)
	}
	if run.PodStartTimeout != runner.DefaultPodStartTimeout {
		t.Errorf("PodStartTimeout = %s, want the default %s",
			run.PodStartTimeout, runner.DefaultPodStartTimeout)
	}

	if err := cmd.ParseFlags([]string{"--pod-start-timeout=45s"}); err != nil {
		t.Fatal(err)
	}
	run, err = f.Runner()
	if err != nil {
		t.Fatalf("Runner: %v", err)
	}
	if run.PodStartTimeout != 45*time.Second {
		t.Errorf("PodStartTimeout = %s, want 45s", run.PodStartTimeout)
	}
}

func TestSnapshotsCommandDefaultsTheHostToTheResticHost(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}

	got, err := snapshotsCommand(sb, &snapshotsOptions{})
	if err != nil {
		t.Fatalf("snapshotsCommand: %v", err)
	}
	if want := "restic snapshots --tag=files --host=" + testNS; strings.Join(got, " ") != want {
		t.Errorf("snapshotsCommand = %v, want %v", got, want)
	}

	sb.Spec.ResticHost = testResticHost
	got, err = snapshotsCommand(sb, &snapshotsOptions{})
	if err != nil {
		t.Fatalf("snapshotsCommand: %v", err)
	}
	if want := "restic snapshots --tag=files --host=" + testResticHost; strings.Join(got, " ") != want {
		t.Errorf("snapshotsCommand = %v, want %v", got, want)
	}
}

func TestSnapshotsCommandHostFlagAndTagFiltering(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}
	sb.Spec.ResticHost = testResticHost

	tests := []struct {
		name string
		o    snapshotsOptions
		want string
	}{
		{"an explicit --host wins", snapshotsOptions{host: "elsewhere", hostSet: true},
			"restic snapshots --tag=files --host=elsewhere"},
		{`--host="" lifts the filter`, snapshotsOptions{hostSet: true},
			"restic snapshots --tag=files"},
		{"--all-tags drops the tags, not the host", snapshotsOptions{allTags: true},
			"restic snapshots --host=" + testResticHost},
		{"--json and --latest are appended last", snapshotsOptions{asJSON: true, latest: 3},
			"restic snapshots --tag=files --host=" + testResticHost + " --json --latest=3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := snapshotsCommand(sb, &tt.o)
			if err != nil {
				t.Fatalf("snapshotsCommand: %v", err)
			}
			if strings.Join(got, " ") != tt.want {
				t.Errorf("snapshotsCommand = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSnapshotsCommandStillRefusesAScriptBackup(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Script = "restic backup /data"

	if _, err := snapshotsCommand(sb, &snapshotsOptions{}); !errors.Is(err, errUnknownSourceTags) {
		t.Fatalf("expected errUnknownSourceTags, got %v", err)
	}
}
