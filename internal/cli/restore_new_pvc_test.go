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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	pvcNS        = "prod"
	pvcBackup    = "web-files"
	pvcRepo      = "store"
	pvcSource    = "app-data"
	pvcNew       = "web-files-inspect"
	pvcMount     = "/app"
	pvcSnapshot  = "4bd2b7d1aa0e9c4c1f3a0f1f1b2c3d4e5f60718293a4b5c6d7e8f90123456789"
	managedByKey = "app.kubernetes.io/managed-by"
	pvcSBUID     = "uid-sb"
)

var (
	errPodNeverStarted = errors.New("pod never started")
	errResticRestore   = errors.New("restic restore failed")
)

func TestRestoreToNewPVCCleansUpAfterItself(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		existing      *corev1.PersistentVolumeClaim
		failStart     bool
		failRestore   bool
		wantErr       error
		wantClaim     bool
		wantSaid      string
		wantNoAttach  bool
		wantClaimSame bool
	}{
		{name: "a restore that succeeds keeps its claim", wantClaim: true, wantSaid: "ready to mount"},
		{name: "a pod that never starts leaves no empty claim", failStart: true, wantErr: errPodNeverStarted,
			wantSaid: "deleted pvc/" + pvcNew},
		{name: "a restore that fails while writing keeps the claim", failRestore: true, wantErr: errResticRestore,
			wantClaim: true, wantSaid: "kubectl delete pvc -n " + pvcNS + " " + pvcNew},
		{name: "a claim an earlier b2bk restore left is not reused",
			existing: &corev1.PersistentVolumeClaim{Namespace: pvcNS, Name: pvcNew,
				Labels: map[string]string{managedByKey: runner.ManagedByValue}},
			wantErr: cli.ErrClaimExists, wantClaim: true, wantClaimSame: true, wantNoAttach: true,
			wantSaid: "kubectl delete pvc -n " + pvcNS + " " + pvcNew},
		{name: "a claim b2bk did not create is not touched",
			existing: &corev1.PersistentVolumeClaim{Namespace: pvcNS, Name: pvcNew},
			wantErr:  cli.ErrClaimExists, wantClaim: true, wantClaimSame: true, wantNoAttach: true,
			wantSaid: "b2bk did not create it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objs := pvcFixtures()
			if tc.existing != nil {
				objs = append(objs, tc.existing)
			}
			c := pvcClient(t, objs...)
			attached := 0
			var errOut bytes.Buffer
			f := cli.NewTestFactory(c, &errOut, func(opts runner.Options) (cli.RestoreExec, error) {
				attached++
				if len(opts.ExtraVolumes) > 0 && tc.failStart {
					return nil, errPodNeverStarted
				}
				return fakeRestic(tc.failRestore), nil
			})
			sb := pvcScheduledBackup()

			err := cli.RestoreToNewPVC(context.Background(), f, sb, pvcNew)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			said := errOut.String()
			if err != nil {
				said += err.Error()
			}
			if !strings.Contains(said, tc.wantSaid) {
				t.Errorf("output %q does not say %q", said, tc.wantSaid)
			}
			if tc.wantNoAttach && attached > 0 {
				t.Errorf("started %d pods before refusing an existing claim", attached)
			}

			var pvc corev1.PersistentVolumeClaim
			err = c.Get(context.Background(), client.ObjectKey{Namespace: pvcNS, Name: pvcNew}, &pvc)
			switch {
			case tc.wantClaim && err != nil:
				t.Fatalf("pvc/%s is gone: %v", pvcNew, err)
			case !tc.wantClaim && !apierrors.IsNotFound(err):
				t.Fatalf("pvc/%s was left behind: %v", pvcNew, err)
			case tc.wantClaimSame && len(pvc.Spec.AccessModes) > 0:
				t.Errorf("the existing claim was rewritten: %+v", pvc.Spec)
			}
		})
	}
}

func fakeRestic(failRestore bool) cli.RestoreExec {
	return func(_ context.Context, argv []string, stdout io.Writer) error {
		switch {
		case len(argv) > 1 && argv[1] == cmdSnapshots:
			_, err := fmt.Fprintf(stdout,
				`[{"id":%q,"time":"2026-10-01T02:17:00Z","paths":[%q],"hostname":%q,"tags":["files"]}]`,
				pvcSnapshot, pvcMount, pvcNS)
			return err
		case len(argv) > 1 && argv[1] == "ls":
			_, err := fmt.Fprintf(stdout, "{\"type\":\"dir\",\"path\":%q}\n", pvcMount)
			return err
		case len(argv) > 1 && argv[1] == cmdRestore && failRestore:
			return errResticRestore
		}
		return nil
	}
}

func pvcScheduledBackup() *b2backupv1.ScheduledBackup {
	return &b2backupv1.ScheduledBackup{
		Namespace: pvcNS, Name: pvcBackup, UID: pvcSBUID,
		Spec: b2backupv1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: pvcRepo},
			Schedule:      "@daily",
			Volumes:       []b2backupv1.VolumeSpec{{ExistingClaim: pvcSource, MountPath: pvcMount}},
			Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}},
		},
	}
}

func pvcFixtures() []client.Object {
	return []client.Object{
		pvcScheduledBackup(),
		&b2backupv1.Repository{Namespace: pvcNS, Name: pvcRepo},
		&corev1.PersistentVolumeClaim{
			Namespace: pvcNS, Name: pvcSource,
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
				},
			},
		},
	}
}

func pvcClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		b2backupv1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}
