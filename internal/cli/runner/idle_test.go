package runner_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

// The idle command runs here with pkill and pgrep stubbed: the real ones would
// signal every restic this user runs. The pgrep stub reports a restic running
// twice, then none.
func TestIdleCommandExitsOnItsOwnSignalHandler(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "SIGTERM", signal: syscall.SIGTERM},
		{name: "SIGINT", signal: syscall.SIGINT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			if err := b2backupv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			repo := &b2backupv1.Repository{Namespace: "prod", Name: "idle", UID: "uid-idle",
				Status: b2backupv1.RepositoryStatus{SecretName: "idle-creds"}}
			r := &runner.Runner{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(repo).Build()}
			job, err := r.BuildFor(context.Background(), runner.Subject{Repository: repo},
				runner.Options{TTY: true, Purpose: "shell"})
			if err != nil {
				t.Fatal(err)
			}
			argv := job.Spec.Template.Spec.Containers[0].Command
			if len(argv) == 0 {
				t.Fatal("the idle pod has no command")
			}

			stubs, logs := t.TempDir(), t.TempDir()
			for name, script := range map[string]string{
				"pkill": "#!/bin/sh\necho \"$@\" >>\"$LOGS/pkill\"\n",
				"pgrep": "#!/bin/sh\nn=$(cat \"$LOGS/pgrep\" 2>/dev/null || echo 0)\necho $((n+1)) >\"$LOGS/pgrep\"\n[ \"$n\" -lt 2 ]\n",
			} {
				if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = append(os.Environ(), "LOGS="+logs, "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()

			time.Sleep(500 * time.Millisecond)
			if err := cmd.Process.Signal(tt.signal); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				var exitErr *exec.ExitError
				if err != nil && !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				if cmd.ProcessState.ExitCode() != 0 {
					t.Errorf("%v ended the idle command with %v; as PID 1 it would ignore the signal",
						argv, cmd.ProcessState)
				}
				if got, _ := os.ReadFile(filepath.Join(logs, "pkill")); string(got) != "-TERM (^|/)restic$\n" {
					t.Errorf("pkill ran with %q, want SIGTERM to restic", got)
				}
				if got, _ := os.ReadFile(filepath.Join(logs, "pgrep")); string(got) != "3\n" {
					t.Errorf("pgrep ran %q times, want until restic was gone", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%v still running 5s after %s", argv, tt.name)
			}
		})
	}
}
