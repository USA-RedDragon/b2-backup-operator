package cli_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	bareNS        = "media"
	bareRepo      = "store"
	clusterImage  = "jacob.pub/ghcr.io/usa-reddragon/restic:0.19.1"
	explicitImage = "example/restic:2"
)

func operatorDeployment(args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		Namespace: "b2-backup-operator-system", Name: "b2-backup-operator-controller-manager",
		Labels: map[string]string{"app.kubernetes.io/name": "b2-backup-operator", "control-plane": "controller-manager"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "manager", Command: []string{"/manager"}, Args: args,
		}}}}},
	}
}

func bareClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		b2backupv1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme, appsv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objs = append(objs, &b2backupv1.Repository{Namespace: bareNS, Name: bareRepo, UID: "uid-store",
		Status: b2backupv1.RepositoryStatus{SecretName: bareRepo + "-credentials"}})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestReadOnlyCommandsRunOnABareRepository(t *testing.T) {
	t.Parallel()
	target := "repo/" + bareRepo
	tests := []struct {
		name      string
		args      []string
		objs      []client.Object
		wantImage string
		wantCmd   string
	}{
		{name: "snapshots", args: []string{cmdSnapshots, target}, wantImage: backup.DefaultImage,
			wantCmd: "restic snapshots"},
		{name: "check with the cluster's image", args: []string{"check", target},
			objs:      []client.Object{operatorDeployment("--leader-elect", "--backup-image="+clusterImage)},
			wantImage: clusterImage, wantCmd: "restic check"},
		{name: "stats with the image as a separate argument", args: []string{"stats", target},
			objs:      []client.Object{operatorDeployment("--backup-image", clusterImage)},
			wantImage: clusterImage, wantCmd: "restic stats --mode=restore-size"},
		{name: "unlock", args: []string{"unlock", target}, wantImage: backup.DefaultImage, wantCmd: "restic unlock"},
		{name: "exec with --image", args: []string{cmdExec, target, "--image", explicitImage, "--", binRestic, "cat", "config"},
			objs:      []client.Object{operatorDeployment("--backup-image=" + clusterImage)},
			wantImage: explicitImage, wantCmd: "restic cat config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var errOut bytes.Buffer
			f := cli.NewTestFactoryIn(bareNS, bareClient(t, tt.objs...), io.Discard, &errOut, nil)
			var got []runner.Subject
			var gotOpts runner.Options
			cli.RunWith(f, func(s runner.Subject, opts runner.Options) error {
				got, gotOpts = append(got, s), opts
				return nil
			})
			if err := cli.Execute(f, tt.args...); err != nil {
				t.Fatalf("%v (stderr %q)", err, errOut.String())
			}
			if len(got) != 1 || got[0].Backup != nil || got[0].Repository == nil || got[0].Repository.Name != bareRepo {
				t.Fatalf("ran for %+v, want repository/%s alone", got, bareRepo)
			}
			if gotOpts.Image != tt.wantImage {
				t.Errorf("image = %q, want %q", gotOpts.Image, tt.wantImage)
			}
			if cmd := strings.Join(gotOpts.Command, " "); cmd != tt.wantCmd {
				t.Errorf("command = %q, want %q", cmd, tt.wantCmd)
			}
			if !strings.Contains(errOut.String(), "has no ScheduledBackup") {
				t.Errorf("stderr %q does not say the Job has only the Repository to go on", errOut.String())
			}
		})
	}
}

func TestWritingCommandsStillNeedAScheduledBackup(t *testing.T) {
	t.Parallel()
	target := "repo/" + bareRepo
	tests := []struct {
		name string
		args []string
	}{
		{name: "prune", args: []string{"prune", target, "--yes"}},
		{name: "restore", args: []string{cmdRestore, target, "--to-new-pvc", "x"}},
		{name: "exec mounting data", args: []string{cmdExec, target, "--mount-data", "--", binRestic, "check"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := cli.NewTestFactoryIn(bareNS, bareClient(t), io.Discard, io.Discard, nil)
			cli.RunWith(f, func(runner.Subject, runner.Options) error {
				t.Error("a Job was started")
				return nil
			})
			if err := cli.Execute(f, tt.args...); !errors.Is(err, cli.ErrNoBackupForRepository) {
				t.Errorf("err = %v, want %v", err, cli.ErrNoBackupForRepository)
			}
		})
	}
}
