package runner_test

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

func TestABareRepositoryGetsAJobOfItsOwn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		opts      runner.Options
		wantImage string
	}{
		{name: "a command", opts: runner.Options{Command: []string{"restic", "check"}, Purpose: "check"},
			wantImage: backup.DefaultImage},
		{name: "a shell", opts: runner.Options{TTY: true, Purpose: "shell"}, wantImage: backup.DefaultImage},
		{name: "another image", opts: runner.Options{Command: []string{"restic", "stats"}, Image: "example/restic:1"},
			wantImage: "example/restic:1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			if err := b2backupv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			repo := &b2backupv1.Repository{Namespace: "prod", Name: "store", UID: "uid-repo",
				Status: b2backupv1.RepositoryStatus{SecretName: "store-creds"}}
			r := &runner.Runner{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(repo).Build()}
			job, err := r.BuildFor(context.Background(), runner.Subject{Repository: repo}, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if owner := metav1.GetControllerOf(job); owner != nil {
				t.Errorf("job is controlled by %+v", owner)
			}
			if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != repo.UID {
				t.Errorf("owners = %+v, want the Repository", job.OwnerReferences)
			}
			if job.Labels[runner.LabelRepository] != repo.Name || job.Spec.Template.Labels[runner.LabelRepository] != repo.Name {
				t.Errorf("labels = %v, pod labels %v", job.Labels, job.Spec.Template.Labels)
			}
			pod := job.Spec.Template.Spec
			if len(pod.Containers) != 1 {
				t.Fatalf("containers = %d", len(pod.Containers))
			}
			c := pod.Containers[0]
			if c.Image != tt.wantImage {
				t.Errorf("image = %q, want %q", c.Image, tt.wantImage)
			}
			if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "store-creds" {
				t.Errorf("envFrom = %+v, want the recorded credentials Secret", c.EnvFrom)
			}
			for _, v := range pod.Volumes {
				if v.EmptyDir == nil {
					t.Errorf("volume %s is not scratch space: %+v", v.Name, v.VolumeSource)
				}
			}
			if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/cache" }) {
				t.Errorf("no cache at /cache: %+v", c.VolumeMounts)
			}
			if tt.opts.TTY != c.TTY {
				t.Errorf("TTY = %t, want %t", c.TTY, tt.opts.TTY)
			}
			if pod.RestartPolicy != corev1.RestartPolicyNever || *job.Spec.BackoffLimit != 0 {
				t.Errorf("restart %s, backoff %d", pod.RestartPolicy, *job.Spec.BackoffLimit)
			}
			if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil ||
				!slices.Contains(c.SecurityContext.Capabilities.Drop, "ALL") || len(c.SecurityContext.Capabilities.Add) > 0 {
				t.Errorf("security context = %+v", c.SecurityContext)
			}
		})
	}
}
