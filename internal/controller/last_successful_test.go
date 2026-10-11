package controller_test

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

const (
	lsNS     = "media"
	lsName   = "media-files"
	lsRepo   = "media"
	lsSBUID  = "uid-media-files"
	lsManual = "media-files-manual-x1y2z"
)

func TestLastSuccessfulTimeCountsManualRuns(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	at := func(ago time.Duration) *metav1.Time { return ptr.To(metav1.NewTime(now.Add(-ago))) }
	cases := []struct {
		name     string
		manual   *metav1.Time
		failed   bool
		cronJob  *metav1.Time
		recorded *metav1.Time
		want     *metav1.Time
	}{
		{name: "only a manual run succeeded", manual: at(time.Hour), want: at(time.Hour)},
		{name: "a manual run after the last scheduled one", manual: at(time.Hour), cronJob: at(3 * time.Hour),
			want: at(time.Hour)},
		{name: "a scheduled run after the last manual one", manual: at(3 * time.Hour), cronJob: at(time.Hour),
			want: at(time.Hour)},
		{name: "a failed manual run", manual: at(time.Hour), failed: true, cronJob: at(3 * time.Hour),
			want: at(3 * time.Hour)},
		{name: "a run that has left the history", recorded: at(2 * time.Hour), cronJob: at(5 * time.Hour),
			want: at(2 * time.Hour)},
		{name: "nothing succeeded yet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := &b2backupv1.ScheduledBackup{
				Namespace: lsNS, Name: lsName, UID: lsSBUID,
				Spec: b2backupv1.ScheduledBackupSpec{
					RepositoryRef: corev1.LocalObjectReference{Name: lsRepo},
					Schedule:      "@hourly",
					Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
				},
				Status: b2backupv1.ScheduledBackupStatus{LastSuccessfulTime: tc.recorded},
			}
			repo := &b2backupv1.Repository{
				Namespace: lsNS, Name: lsRepo,
				Status: b2backupv1.RepositoryStatus{Initialized: true, BucketID: "b1", BucketName: "acme"},
			}
			objs := []client.Object{sb, repo}
			if tc.manual != nil {
				objs = append(objs, manualJob(sb, tc.manual, tc.failed))
			}
			r, c := lastSuccessfulHarness(t, objs...)
			ctx := context.Background()
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sb)}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if tc.cronJob != nil {
				var cj batchv1.CronJob
				if err := c.Get(ctx, client.ObjectKey{Namespace: lsNS, Name: backup.CronJobName(sb)}, &cj); err != nil {
					t.Fatal(err)
				}
				cj.Status.LastSuccessfulTime = tc.cronJob
				cj.Status.LastScheduleTime = tc.cronJob
				if err := c.Status().Update(ctx, &cj); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}

			var got b2backupv1.ScheduledBackup
			if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == nil && got.Status.LastSuccessfulTime != nil:
				t.Errorf("lastSuccessfulTime = %v, want none", got.Status.LastSuccessfulTime)
			case tc.want != nil && (got.Status.LastSuccessfulTime == nil || !got.Status.LastSuccessfulTime.Equal(tc.want)):
				t.Errorf("lastSuccessfulTime = %v, want %v", got.Status.LastSuccessfulTime, tc.want)
			}
			if !equalTimes(got.Status.LastScheduleTime, tc.cronJob) {
				t.Errorf("lastScheduleTime = %v, want the CronJob's %v", got.Status.LastScheduleTime, tc.cronJob)
			}
		})
	}
}

func equalTimes(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(b)
}

func manualJob(sb *b2backupv1.ScheduledBackup, finished *metav1.Time, failed bool) *batchv1.Job {
	start := metav1.NewTime(finished.Add(-5 * time.Minute))
	job := &batchv1.Job{
		Namespace: lsNS, Name: lsManual,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "b2-backup-operator",
			backup.LabelTrigger:            backup.TriggerManual,
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: b2backupv1.GroupVersion.String(), Kind: "ScheduledBackup",
			Name: sb.Name, UID: sb.UID, Controller: ptr.To(true),
		}},
		Status: batchv1.JobStatus{StartTime: &start},
	}
	if failed {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: *finished,
		}}
		return job
	}
	job.Status.Succeeded = 1
	job.Status.CompletionTime = finished
	return job
}

func lastSuccessfulHarness(t *testing.T, objs ...client.Object) (*controller.ScheduledBackupReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := b2backupv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&b2backupv1.ScheduledBackup{}, &b2backupv1.Repository{}).Build()
	return &controller.ScheduledBackupReconciler{
		Client: c, Scheme: scheme, Recorder: events.NewFakeRecorder(100),
		Config: backup.Config{Image: "ghcr.io/usa-reddragon/restic:test"},
	}, c
}
