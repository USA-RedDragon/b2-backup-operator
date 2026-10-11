package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	argsNS     = "apps"
	cmdStatus  = "status"
	cmdSuspend = "suspend"
	cmdPrune   = "prune"
	cmdUnlock  = "unlock"
	argsApp01  = "app-01"
	usingApp01 = "using " + "scheduledbackup/" + argsApp01 + "\n"
)

func TestWrongArgumentCountsNameTheUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"version", "x"}, want: "version takes no arguments, got 1: b2bk version"},
		{args: []string{"parity", "a.yaml"}, want: "parity takes exactly two arguments, got 1: b2bk parity <generated.yaml>"},
		{args: []string{"validate"}, want: "validate takes exactly one argument, got none: b2bk validate <file.yaml>"},
		{args: []string{"migrate", "a", "b"}, want: "migrate takes exactly one argument, got 2: b2bk migrate"},
		{args: []string{"get", "sb", "repo"}, want: "get takes at most one argument, got 2: b2bk get [repositories|backups]"},
		{args: []string{cmdRestore, "a", "b"}, want: "restore takes exactly one name, got 2: b2bk restore"},
		{args: []string{cmdExec, "a", binRestic}, want: "exec needs -- before the command: b2bk exec"},
		{args: []string{cmdExec, "a", "b", "--", binRestic}, want: "exec takes at most one name before --, got 2: b2bk exec"},
		{args: []string{cmdStatus, "a", "b"}, want: "status takes at most one name, got 2: b2bk status [NAME]"},
		{args: []string{cmdSuspend, "a", "b"}, want: "suspend takes at most one name, got 2: b2bk suspend [NAME]"},
		{args: []string{cmdExec, "a", "--"}, want: "exec needs a command after --: b2bk exec"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			f := cli.NewTestFactoryIn(argsNS, nil, io.Discard, io.Discard, nil)
			err := cli.Execute(f, tt.args...)
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if !errors.Is(err, cli.ErrUsage) || !strings.HasPrefix(err.Error(), tt.args[0]+" ") {
				t.Errorf("error %q is not a usage error that starts with the command", err)
			}
			if strings.Contains(err.Error(), "arg(s)") {
				t.Errorf("error %q is cobra's", err)
			}
		})
	}
}

const argsRepo = "vault"

func argsClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return argsClientBuilder(t, objs...).Build()
}

func argsClientBuilder(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		b2backupv1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
}

func argsRepository(ns, name string) *b2backupv1.Repository {
	return &b2backupv1.Repository{Namespace: ns, Name: name, UID: types.UID("uid-repo-" + name),
		Status: b2backupv1.RepositoryStatus{SecretName: name + "-credentials"}}
}

func argsBackup(ns, name string) *b2backupv1.ScheduledBackup {
	return &b2backupv1.ScheduledBackup{Namespace: ns, Name: name, UID: types.UID("uid-sb-" + name),
		Spec: b2backupv1.ScheduledBackupSpec{RepositoryRef: corev1.LocalObjectReference{Name: argsRepo}}}
}

func argsBackups(n int) []client.Object {
	objs := []client.Object{argsRepository(argsNS, argsRepo)}
	for i := n; i > 0; i-- {
		objs = append(objs, argsBackup(argsNS, fmt.Sprintf("app-%02d", i)))
	}
	return objs
}

var execCatConfig = []string{cmdExec, "--", binRestic, "cat", "config"}

func TestNoNameUsesTheOnlyOneInTheNamespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		objs       []client.Object
		wantErr    error
		wantStderr string
		wantOut    string
		wantText   []string
		wantRun    string
	}{
		{name: "status with no backup or repository", args: []string{cmdStatus},
			objs:    []client.Object{argsBackup("elsewhere", "app"), argsRepository("elsewhere", argsRepo)},
			wantErr: cli.ErrNothingToName,
			wantText: []string{`no ScheduledBackup or Repository in namespace "apps"`, "-n NAMESPACE",
				"b2bk get -A"}},
		{name: "status with one backup", args: []string{cmdStatus}, objs: argsBackups(1),
			wantStderr: usingApp01, wantOut: "scheduledbackup/app-01\n"},
		{name: "status with two backups", args: []string{cmdStatus}, objs: argsBackups(2),
			wantErr: cli.ErrNameNeeded,
			wantText: []string{`namespace "apps" has 2 ScheduledBackups; pick one, ` +
				"e.g. b2bk status scheduledbackup/app-01\n  scheduledbackup/app-01\n  scheduledbackup/app-02"}},
		{name: "status with a repository alone", args: []string{cmdStatus},
			objs:       []client.Object{argsRepository(argsNS, argsRepo)},
			wantStderr: "using repository/vault\n", wantOut: "repository/vault\n"},
		{name: "status with two repositories alone", args: []string{cmdStatus},
			objs:    []client.Object{argsRepository(argsNS, "b"), argsRepository(argsNS, "a")},
			wantErr: cli.ErrNameNeeded,
			wantText: []string{`namespace "apps" has 2 Repositories; pick one, ` +
				"e.g. b2bk status repository/a\n  repository/a\n  repository/b"}},
		{name: "a dozen backups are capped", args: []string{cmdStatus}, objs: argsBackups(12),
			wantErr: cli.ErrNameNeeded, wantText: []string{"scheduledbackup/app-10\n  and 2 more"}},
		{name: "logs takes no repository", args: []string{"logs"},
			objs:     []client.Object{argsRepository(argsNS, argsRepo)},
			wantErr:  cli.ErrNothingToName,
			wantText: []string{`no ScheduledBackup in namespace "apps"`}},
		{name: "suspend", args: []string{cmdSuspend}, objs: argsBackups(1),
			wantStderr: usingApp01, wantOut: "scheduledbackup/app-01 suspended\n"},
		{name: "exec with nothing before --", args: execCatConfig,
			objs: argsBackups(1), wantStderr: usingApp01, wantRun: "app-01"},
		{name: "exec example keeps the command", args: execCatConfig,
			objs: argsBackups(2), wantErr: cli.ErrNameNeeded,
			wantText: []string{"e.g. b2bk exec scheduledbackup/app-01 -- <command>..."}},
		{name: "snapshots", args: []string{cmdSnapshots, "--all-tags"}, objs: argsBackups(1),
			wantStderr: usingApp01, wantRun: "app-01"},
		{name: "a name is still used as given", args: []string{cmdStatus, "app-02"}, objs: argsBackups(2),
			wantOut: "scheduledbackup/app-02\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			f := cli.NewTestFactoryIn(argsNS, argsClient(t, tt.objs...), &out, &errOut, nil)
			var ran string
			cli.RunWith(f, func(s runner.Subject, _ runner.Options) error {
				ran = s.Backup.Name
				return nil
			})
			err := cli.Execute(f, tt.args...)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v (stderr %q)", err, tt.wantErr, errOut.String())
			}
			for _, want := range tt.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if tt.wantErr != nil {
				return
			}
			if !strings.HasPrefix(errOut.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to start with %q", errOut.String(), tt.wantStderr)
			}
			if tt.wantStderr == "" && strings.Contains(errOut.String(), "using ") {
				t.Errorf("stderr %q says it picked one although a name was given", errOut.String())
			}
			if !strings.HasPrefix(out.String(), tt.wantOut) {
				t.Errorf("stdout = %q, want it to start with %q", out.String(), tt.wantOut)
			}
			if ran != tt.wantRun {
				t.Errorf("ran for %q, want %q", ran, tt.wantRun)
			}
		})
	}
}

var errForbidden = errors.New("forbidden")

func TestDestructiveCommandsNeedANameEvenForTheOnlyOne(t *testing.T) {
	t.Parallel()
	listed := "namespace \"apps\" has:\n  scheduledbackup/app-01"
	both := listed + "\n  repository/vault"
	tests := []struct {
		args     []string
		objs     []client.Object
		denyList bool
		want     []string
	}{
		{args: []string{cmdRestore, "--to", "x"}, objs: argsBackups(1),
			want: []string{"restore needs a name, even when there is only one, since it can overwrite live data: " +
				"b2bk restore NAME\n" + listed}},
		{args: []string{cmdPrune, "--yes"}, objs: argsBackups(1),
			want: []string{"prune needs a name", "deletes snapshots: b2bk prune NAME\n" + listed}},
		{args: []string{cmdUnlock}, objs: argsBackups(1), want: []string{"b2bk unlock NAME\n" + both}},
		{args: []string{"rotate-password", "--i-have-somewhere-to-archive-the-new-password"}, objs: argsBackups(1),
			want: []string{"b2bk rotate-password REPOSITORY\n" + both}},
		{args: []string{"reinit"}, objs: argsBackups(1),
			want: []string{"b2bk reinit REPOSITORY\nnamespace \"apps\" has:\n  repository/vault"}},
		{args: []string{"cancel"}, objs: argsBackups(1), want: []string{"b2bk cancel NAME\n" + listed}},
		{args: []string{"resume"}, objs: argsBackups(1), want: []string{"b2bk resume NAME\n" + both}},
		{args: []string{cmdRestore}, want: []string{"b2bk restore NAME\n" + `namespace "apps" has no ScheduledBackup`}},
		{args: []string{cmdRestore}, objs: argsBackups(1), denyList: true, want: []string{"b2bk restore NAME"}},
		{args: []string{cmdRestore, "a", "b"}, objs: argsBackups(1),
			want: []string{"restore takes exactly one name, got 2: b2bk restore NAME"}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s %t", strings.Join(tt.args, " "), tt.denyList), func(t *testing.T) {
			t.Parallel()
			b := argsClientBuilder(t, tt.objs...)
			if tt.denyList {
				b = b.WithInterceptorFuncs(interceptor.Funcs{
					List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
						return errForbidden
					},
				})
			}
			var errOut bytes.Buffer
			f := cli.NewTestFactoryIn(argsNS, b.Build(), io.Discard, &errOut, nil)
			cli.RunWith(f, func(runner.Subject, runner.Options) error {
				t.Error("a Job was started")
				return nil
			})
			err := cli.Execute(f, tt.args...)
			if !errors.Is(err, cli.ErrUsage) {
				t.Fatalf("err = %v, want %v", err, cli.ErrUsage)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if tt.denyList && strings.Contains(err.Error(), "namespace") {
				t.Errorf("error %q lists although listing failed", err)
			}
			if errOut.Len() > 0 {
				t.Errorf("stderr = %q, want nothing", errOut.String())
			}
		})
	}
}
