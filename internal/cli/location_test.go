package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

const (
	locBucket = "acme-media"
	locRepo   = "media"
	cmdDoctor = "doctor"
)

func TestRepositoryLocationIsNeverJoinedOntoAnEmptyPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		want     []string
		wantNone []string
	}{
		{name: "bucket mode at the root", path: "",
			want:     []string{"stored at " + locBucket + " (bucket root)", "(bucket root)"},
			wantNone: []string{locBucket + "/ ", locBucket + "//"}},
		{name: "prefix mode", path: "restic/prod/media",
			want: []string{"stored at " + locBucket + "/restic/prod/media"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := locationRepo(tc.path)
			c := pvcClient(t, repo, locationSecret(repo))
			ctx := context.Background()

			var doctor, status, get bytes.Buffer
			_ = cli.Doctor(ctx, c, &doctor, pvcNS, "repo/"+locRepo)
			if err := cli.Status(ctx, c, &status, pvcNS, "repo/"+locRepo, 5); err != nil {
				t.Fatal(err)
			}
			if err := cli.PrintRepositories(&get, []b2backupv1.Repository{*repo}, false, ""); err != nil {
				t.Fatal(err)
			}
			for name, out := range map[string]string{cmdDoctor: doctor.String(), "status": status.String(), "get": get.String()} {
				for _, bad := range tc.wantNone {
					if strings.Contains(out, bad) {
						t.Errorf("%s output joins the bucket onto an empty path (%q):\n%s", name, bad, out)
					}
				}
				if tc.path != "" && !strings.Contains(out, tc.path) {
					t.Errorf("%s output does not show the path %s:\n%s", name, tc.path, out)
				}
				if tc.path == "" && name != cmdDoctor && !strings.Contains(out, "(bucket root)") {
					t.Errorf("%s output does not say the repository is at the bucket root:\n%s", name, out)
				}
			}
			if !strings.Contains(doctor.String(), tc.want[0]) {
				t.Errorf("doctor does not say %q:\n%s", tc.want[0], doctor.String())
			}
		})
	}
}

func locationRepo(path string) *b2backupv1.Repository {
	mode := b2backupv1.StorageModeBucket
	if path != "" {
		mode = b2backupv1.StorageModePrefix
	}
	return &b2backupv1.Repository{
		Namespace: pvcNS, Name: locRepo, Generation: 1,
		Spec: b2backupv1.RepositorySpec{Mode: mode, Bucket: locBucket, Path: path},
		Status: b2backupv1.RepositoryStatus{
			ObservedGeneration: 1,
			BucketID:           "bucket-id",
			BucketName:         locBucket,
			Path:               path,
			Initialized:        true,
			Conditions: []metav1.Condition{{
				Type: b2backupv1.RepositoryConditionReady, Status: metav1.ConditionTrue, Reason: "Ready",
				ObservedGeneration: 1,
			}},
		},
	}
}

func locationSecret(repo *b2backupv1.Repository) *corev1.Secret {
	data := map[string][]byte{}
	for _, k := range controller.SecretKeys {
		data[k] = []byte("x")
	}
	return &corev1.Secret{Namespace: pvcNS, Name: repo.SecretName(), Data: data}
}
