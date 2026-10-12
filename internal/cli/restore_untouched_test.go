package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	utilexec "k8s.io/client-go/util/exec"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/tui"
)

const (
	resumeHint     = "b2bk resume"
	readDump       = "READ=1"
	bigDump        = "DUMP_BYTES=4194304"
	nothingWritten = "nothing was written"
)

// exitPod fails the restoredb pipeline with err.
type exitPod struct {
	fakePod
	err error
}

func (p *exitPod) exec(ctx context.Context, argv []string, stdout io.Writer) error {
	if line := strings.Join(argv, " "); strings.Contains(line, purposeRestoreDB) {
		p.commands = append(p.commands, line)
		return p.err
	}
	return p.fakePod.exec(ctx, argv, stdout)
}

func exitStatus(code int) error {
	return utilexec.CodeExitError{Err: fmt.Errorf("command terminated with non-zero exit code: exit code %d", code),
		Code: code}
}

func TestADatabaseRestoreThatRestoredbStoppedBeforeWritingIsNotHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		earlier   bool
		untouched bool
		partial   int
		says      []string
		never     string
	}{
		{name: "restoredb stopped in its checks", err: exitStatus(restoredbUntouchedExit), untouched: true,
			says: []string{nothingWritten, "released"}, never: resumeHint},
		{name: "restoredb stopped in its checks after an earlier partial restore",
			err: exitStatus(restoredbUntouchedExit), earlier: true, untouched: true, partial: 1,
			says: []string{"may hold a partial restore", resumeHint}, never: nothingWritten},
		{name: "restoredb failed in the restore", err: exitStatus(1), partial: 1,
			says: []string{"may hold a partial restore", resumeHint}, never: nothingWritten},
		{name: "psql failed in the restore", err: exitStatus(3), partial: 1,
			says: []string{resumeHint}, never: nothingWritten},
		{name: "the exec stream broke", err: errors.New("connection reset by peer"), partial: 1,
			says: []string{resumeHint}, never: nothingWritten},
	}
	paths := []struct {
		name string
		run  func(*Factory, client.Client, *b2backupv1.ScheduledBackup, *bytes.Buffer) (string, error)
	}{
		{"b2bk restore", func(f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup,
			errOut *bytes.Buffer) (string, error) {
			err := restoreToDatabase(context.Background(), f, c, sb,
				&restoreOptions{snapshot: latest, yes: true, toDatabase: true})
			return errOut.String(), err
		}},
		{"the TUI", func(f *Factory, _ client.Client, _ *b2backupv1.ScheduledBackup,
			_ *bytes.Buffer) (string, error) {
			var out bytes.Buffer
			err := (&tuiBackend{f: f}).Restore(context.Background(), tui.RestoreRequest{
				Namespace: testNS, Backup: testBackupName, Target: tui.TargetDatabase, Confirm: testBackupName,
			}, &out)
			return out.String(), err
		}},
	}
	for _, path := range paths {
		for _, tt := range tests {
			t.Run(path.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, cnpgOnly)
				if tt.earlier {
					raw, err := json.Marshal([]string{databaseKey(fx.sb, "")})
					if err != nil {
						t.Fatal(err)
					}
					fx.sb.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: string(raw)}
				}
				c := newClient(t, fx.objects()...)
				pod := &exitPod{err: tt.err}
				f, errOut := hookedFactory(t, c, pod, true)
				stderr, err := path.run(f, c, fx.sb, errOut)
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want the pipeline's failure", err)
				}
				if got := errors.Is(err, ErrNothingWritten); got != tt.untouched {
					t.Errorf("ErrNothingWritten = %t, want %t: %v", got, tt.untouched, err)
				}
				if tt.untouched && !strings.Contains(err.Error(), "was not changed") {
					t.Errorf("err does not say the database was not changed: %v", err)
				}
				if !strings.Contains(strings.Join(pod.commands, "\n"), purposeRestoreDB) {
					t.Fatalf("restoredb never ran: %v", pod.commands)
				}
				lock, partial := held(t, c, testBackupName)
				if lock != "" || len(partial) != tt.partial {
					t.Errorf("lock = %q, partial = %v; want no lock and %d entries", lock, partial, tt.partial)
				}
				for _, s := range tt.says {
					if !strings.Contains(stderr, s) {
						t.Errorf("stderr does not say %q:\n%s", s, stderr)
					}
				}
				if strings.Contains(stderr, tt.never) {
					t.Errorf("stderr says %q:\n%s", tt.never, stderr)
				}
			})
		}
	}
}

// The pod runs the pipeline under set -o pipefail, which gives the status of
// the last command that failed, so restoredb's status wins whenever it failed,
// whatever restic did. restic is never left writing to a closed pipe, which
// would kill it before it removes its lock, and is sent SIGTERM when restoredb
// stops before reading the dump.
func TestRestoreDatabaseCommandExitsWithRestoredbsStatus(t *testing.T) {
	t.Parallel()
	shell := pipefailShell()
	if shell == nil {
		t.Skip("neither busybox nor bash is installed")
	}
	bin := t.TempDir()
	scripts := map[string]string{
		resticController: "#!/bin/sh\ntrap 'echo term >\"$OUT.term\"' TERM\nhead -c \"${DUMP_BYTES:-0}\" /dev/zero\n" +
			"echo \"$?\" >\"$OUT.write\"\nexit \"${RESTIC_EXIT:-0}\"\n",
		purposeRestoreDB: "#!/bin/sh\n[ -n \"$READ\" ] && cat >/dev/null\nexit \"${RESTOREDB_EXIT:-0}\"\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sb := newBackup(testBackupName, testRepoName)
	withDatabase(sb)
	argv := restoreDatabaseCommand(sb, resticSnapshot{ID: snapDB, Paths: []string{"/" + testNS + ".dmp"}}, "", false)

	tests := []struct {
		name       string
		env        []string
		want       int
		terminated bool
	}{
		{"both succeed", []string{readDump}, 0, false},
		{"restoredb stops in its checks, before reading", []string{bigDump, "RESTOREDB_EXIT=75"}, 75, true},
		{"restoredb succeeds without reading it all", []string{bigDump}, 0, false},
		{"restic fails and restoredb finds no dump", []string{"RESTIC_EXIT=1", readDump, "RESTOREDB_EXIT=75"}, 75, false},
		{"restoredb fails in the restore", []string{bigDump, readDump, "RESTOREDB_EXIT=1"}, 1, false},
		{"psql fails in the restore", []string{readDump, "RESTOREDB_EXIT=3"}, 3, false},
		{"restic fails after restoredb succeeded", []string{"RESTIC_EXIT=12", readDump}, 12, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(shell[0], append(shell[1:], argv[1:]...)...)
			out := filepath.Join(t.TempDir(), "restic")
			cmd.Env = append([]string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "OUT=" + out},
				tt.env...)
			err := cmd.Run()
			got := 0
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
				got = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("exit %d, want %d", got, tt.want)
			}
			wrote, err := os.ReadFile(out + ".write")
			if err != nil || strings.TrimSpace(string(wrote)) != "0" {
				t.Errorf("restic did not write the whole dump: status %q, %v", wrote, err)
			}
			if _, err := os.Stat(out + ".term"); tt.terminated && err != nil {
				t.Errorf("restic was not sent SIGTERM: %v", err)
			}
		})
	}
}

// pipefailShell is a shell with set -o pipefail: busybox sh, as in the backup
// image, else bash. dash, the sh of Debian and Ubuntu, does not have it.
func pipefailShell() []string {
	if _, err := exec.LookPath("busybox"); err == nil {
		return []string{"busybox", "sh"}
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return []string{"bash"}
	}
	return nil
}
