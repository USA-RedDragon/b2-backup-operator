package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/tui"
)

// tuiFactory is a Factory whose terminal is decided by the test, and which
// records the TUI it would open instead of opening one.
func tuiFactory(t *testing.T, terminal bool) (*Factory, *bytes.Buffer, *[]tui.Options) {
	t.Helper()
	var out bytes.Buffer
	f := offlineFactory(t, testStreams())
	f.Streams.Out = &out
	f.interactive = ptr.To(terminal)
	var opened []tui.Options
	f.tuiHook = func(_ context.Context, opts tui.Options) error {
		opened = append(opened, opts)
		return nil
	}
	return f, &out, &opened
}

func TestBareB2bkPrintsHelpWithoutATerminal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	f, out, opened := tuiFactory(t, false)
	if err := Execute(f); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 0 {
		t.Fatal("b2bk opened the TUI without a terminal")
	}
	for _, want := range []string{"Usage:", "Inspect:", "doctor", "b2bk tui --help"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help is missing %q:\n%s", want, out.String())
		}
	}
}

func TestBareB2bkPrintsHelpOnADumbTerminal(t *testing.T) {
	t.Setenv("TERM", "dumb")
	f := offlineFactory(t, testStreams())
	if f.canDrawTUI() {
		t.Error("streams that are not files count as a terminal")
	}
}

func TestBareB2bkOpensTheTUIOnATerminal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(tui.EnvAllNamespaces, "")
	t.Setenv(tui.EnvMouse, "")
	t.Setenv(tui.EnvNoColor, "")
	t.Setenv(tui.EnvCLIColor, "")
	tests := []struct {
		name string
		args []string
		env  map[string]string
		file string
		want tui.Options
	}{
		{"defaults", nil, nil, "", tui.Options{Mouse: true}},
		{"-A", []string{"-A"}, nil, "", tui.Options{AllNamespaces: true, Mouse: true}},
		{"tui -A", []string{"tui", "--all-namespaces"}, nil, "", tui.Options{AllNamespaces: true, Mouse: true}},
		{"config file", nil, nil, "tui:\n  allNamespaces: true\n  mouse: false\n", tui.Options{AllNamespaces: true}},
		{"env over the file", nil, map[string]string{tui.EnvMouse: "true"}, "tui:\n  mouse: false\n",
			tui.Options{Mouse: true}},
		{"flags over the env", []string{"--mouse=false", "-A"},
			map[string]string{tui.EnvMouse: "true", tui.EnvAllNamespaces: "false"}, "",
			tui.Options{AllNamespaces: true}},
		{"NO_COLOR", nil, map[string]string{tui.EnvNoColor: "yes"}, "", tui.Options{Mouse: true, NoColor: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, "b2bk", "config.yaml")
			_ = os.Remove(path)
			if tt.file != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			f, _, opened := tuiFactory(t, true)
			if err := Execute(f, tt.args...); err != nil {
				t.Fatal(err)
			}
			if len(*opened) != 1 || (*opened)[0] != tt.want {
				t.Errorf("opened %+v, want %+v", *opened, tt.want)
			}
		})
	}
}

func TestTUICommandNeedsATerminal(t *testing.T) {
	f, _, opened := tuiFactory(t, false)
	if err := Execute(f, "tui"); !errors.Is(err, tui.ErrNoTerminal) {
		t.Errorf("err = %v, want ErrNoTerminal", err)
	}
	if len(*opened) != 0 {
		t.Error("b2bk tui opened without a terminal")
	}
}

func TestBareB2bkStillRefusesAnUnknownCommand(t *testing.T) {
	f, _, opened := tuiFactory(t, true)
	err := Execute(f, "statsu")
	if err == nil || !strings.Contains(err.Error(), `unknown command "statsu" for "b2bk"`) ||
		!strings.Contains(err.Error(), "status") {
		t.Errorf("err = %v, want an unknown command with a suggestion", err)
	}
	if len(*opened) != 0 {
		t.Error("an unknown command opened the TUI")
	}
}

func TestTUIOverwritingRestoreNeedsTheTypedName(t *testing.T) {
	t.Parallel()
	targets := []struct {
		name   string
		mutate func(*b2backupv1.ScheduledBackup)
		target tui.Target
		writes string
	}{
		{targetInPlace, filesAtData, tui.TargetInPlace, resticRestore},
		{"database", cnpgOnly, tui.TargetDatabase, "restoredb"},
	}
	for _, target := range targets {
		for _, typed := range []string{"", "web-", testBackupName + " ", testNS, testBackupName} {
			t.Run(target.name+"/"+typed, func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				pod := &fakePod{}
				f, _ := hookedFactory(t, c, pod, true)
				var out bytes.Buffer
				err := (&tuiBackend{f: f}).Restore(context.Background(), tui.RestoreRequest{
					Namespace: testNS, Backup: testBackupName, Target: target.target, Confirm: typed,
				}, &out)
				wrote := slices.ContainsFunc(pod.commands, func(c string) bool { return strings.Contains(c, target.writes) })
				if typed == testBackupName {
					if err != nil || !wrote {
						t.Fatalf("err = %v, wrote = %t; the typed name did not confirm", err, wrote)
					}
					return
				}
				if !errors.Is(err, ErrNotConfirmed) {
					t.Fatalf("err = %v, want ErrNotConfirmed", err)
				}
				if wrote {
					t.Errorf("an unconfirmed restore ran %v", pod.commands)
				}
				got := backupState(t, c)
				if got.Annotations[b2backupv1.AnnotationRestoreLock] != "" || got.Spec.Suspend {
					t.Errorf("an unconfirmed restore left the backup held: %v suspend=%t", got.Annotations, got.Spec.Suspend)
				}
			})
		}
	}
}

func TestTUIRestoresKeepTheCLIGuards(t *testing.T) {
	t.Parallel()
	for _, tc := range stalenessCases() {
		if tc.wants == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := currentBackup(t, cnpgOnly)
			extra := tc.edit(&fx)
			c := newClient(t, fx.objects(extra...)...)
			pod := &fakePod{}
			f, _ := hookedFactory(t, c, pod, true)
			b := &tuiBackend{f: f}
			err := b.Restore(context.Background(), tui.RestoreRequest{
				Namespace: testNS, Backup: testBackupName, Target: tui.TargetDatabase, Confirm: testBackupName,
			}, io.Discard)
			checkStalenessError(t, err, tc.wants)
			if len(pod.commands) > 0 {
				t.Errorf("a refused restore ran %v", pod.commands)
			}
			why := b.RestoreTargets(context.Background(), testNS, testBackupName, "db")
			if !strings.Contains(why[tui.TargetDatabase], tc.wants) {
				t.Errorf("RestoreTargets says %q for the database, want %q", why[tui.TargetDatabase], tc.wants)
			}
		})
	}
}

func TestTUIRestoreTargetsForEachSource(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t, filesAtData)
	c := newClient(t, fx.objects()...)
	f, _ := hookedFactory(t, c, &fakePod{}, false)
	why := (&tuiBackend{f: f}).RestoreTargets(context.Background(), testNS, testBackupName, "")
	if why[tui.TargetNewClaim] != "" || why[tui.TargetDownload] != "" || why[tui.TargetInPlace] != "" {
		t.Errorf("a current files backup cannot use %v", why)
	}
	if why[tui.TargetDatabase] != "this backup has no database" {
		t.Errorf("database = %q", why[tui.TargetDatabase])
	}

	db := currentBackup(t, cnpgOnly)
	c = newClient(t, db.objects()...)
	f, _ = hookedFactory(t, c, &fakePod{}, false)
	why = (&tuiBackend{f: f}).RestoreTargets(context.Background(), testNS, testBackupName, "db")
	if why[tui.TargetDatabase] != "" || why[tui.TargetInPlace] == "" || why[tui.TargetNewClaim] == "" {
		t.Errorf("a database backup: %v", why)
	}
}

func TestTUIDryRunOfANewClaimCreatesNoClaim(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t, filesAtData)
	c := newClient(t, fx.objects(sourceClaim())...)
	pod := &fakePod{}
	f, _ := hookedFactory(t, c, pod, false)
	err := (&tuiBackend{f: f}).Restore(context.Background(), tui.RestoreRequest{
		Namespace: testNS, Backup: testBackupName, Target: tui.TargetNewClaim, Claim: "web-files-check", DryRun: true,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := backupState(t, c); got.Annotations[b2backupv1.AnnotationRestoreLock] != "" {
		t.Error("a dry run held the backup")
	}
	if !slices.ContainsFunc(pod.commands, func(c string) bool { return strings.Contains(c, "restic ls --long") }) {
		t.Errorf("the dry run did not list the snapshot: %v", pod.commands)
	}
	if claimExists(t, c, "web-files-check") {
		t.Error("the dry run of a new claim created the claim")
	}
}

func TestTUISessionPodListsSnapshotsAndCloses(t *testing.T) {
	t.Parallel()
	sb := filesBackupAt()
	c := newClient(t, sb)
	pod := &fakePod{}
	f, _ := hookedFactory(t, c, pod, false)
	b := &tuiBackend{f: f}
	p, err := b.OpenPod(context.Background(), testNS, testBackupName, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := b.Snapshots(context.Background(), p, testNS, testBackupName)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) == 0 {
		t.Fatalf("no snapshots: %v", pod.commands)
	}
	if pod.commands[0] != "restic snapshots --tag=files --host="+testNS+" --json --group-by=" {
		t.Errorf("snapshots ran %q, not b2bk snapshots --json", pod.commands[0])
	}
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	<-done
}

func TestDoctorNextSteps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		chk  check
		want []tui.Step
	}{
		{check{level: levelFail, detail: []string{"Run: b2bk logs sb/x"}}, []tui.Step{tui.StepLogs, tui.StepBackup}},
		{check{level: levelWarn, detail: []string{"The first run happens at the next scheduled slot.",
			"To take one now: b2bk backup sb/x"}}, []tui.Step{tui.StepBackup}},
		{check{level: levelWarn, detail: []string{"The CronJob is kept but will not fire. Clear spec.suspend to resume."}},
			[]tui.Step{tui.StepResume}},
		{check{level: levelOK}, nil},
	}
	for _, tt := range tests {
		if got := nextSteps(tt.chk); !slices.Equal(got, tt.want) {
			t.Errorf("nextSteps(%v) = %v, want %v", tt.chk.detail, got, tt.want)
		}
	}
}

func claimExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &pvc)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestTUIDownloadsADumpButNotAFolderOfIt(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t, cnpgOnly)
	c := newClient(t, fx.objects()...)
	pod := &fakePod{dump: []byte("dump")}
	f, _ := hookedFactory(t, c, pod, false)
	b := &tuiBackend{f: f}
	p, err := b.OpenPod(context.Background(), testNS, testBackupName, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	dir := t.TempDir()
	req := tui.DownloadRequest{Namespace: testNS, Backup: testBackupName, Snapshot: snapDB, Tag: "db",
		Path: "/" + testNS + ".dmp", Dir: dir}
	if err := b.Download(context.Background(), p, req, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, testNS+".dmp"))
	if err != nil || string(got) != "dump" {
		t.Errorf("downloaded %q, %v", got, err)
	}
	req.Folder, req.Path = true, "/"
	if err := b.Download(context.Background(), p, req, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "restic dump") {
		t.Errorf("err = %v, want a folder of a dump refused", err)
	}
}
