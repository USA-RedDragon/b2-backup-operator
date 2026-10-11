package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const triggerAt = "2026-09-03T14:12:00Z"

func triggeredBackup(at string, mutate func(*b2backupv1.ScheduledBackup)) *b2backupv1.ScheduledBackup {
	sb := scheduledBackup()
	sb.Annotations = map[string]string{b2backupv1.AnnotationTriggerAt: at}
	if mutate != nil {
		mutate(sb)
	}
	return sb
}

func loadBackup(t *testing.T, c client.Client) *b2backupv1.ScheduledBackup {
	t.Helper()
	var sb b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
		t.Fatal(err)
	}
	return &sb
}

func manualJobs(t *testing.T, c client.Client) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	var out []batchv1.Job
	for i := range jobs.Items {
		if jobs.Items[i].Labels[backup.LabelTrigger] == backup.TriggerManual {
			out = append(out, jobs.Items[i])
		}
	}
	return out
}

func TestTriggerCreatesOneJob(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), triggeredBackup(triggerAt, nil))
	reconcileBackup(t, r)

	jobs := manualJobs(t, c)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 manual Job, got %d", len(jobs))
	}

	at, _ := time.Parse(time.RFC3339, triggerAt)
	if want := backup.ManualJobName(scheduledBackup(), at); jobs[0].Name != want {
		t.Errorf("job name = %q, want %q", jobs[0].Name, want)
	}

	if got := jobs[0].Labels["app.kubernetes.io/managed-by"]; got != "b2-backup-operator" {
		t.Errorf("managed-by = %q, want b2-backup-operator", got)
	}

	if owner := metav1.GetControllerOf(&jobs[0]); owner == nil || owner.Kind != kindScheduledBackup {
		t.Errorf("expected a ScheduledBackup owner, got %+v", owner)
	}

	sb := loadBackup(t, c)
	if sb.Status.LastTriggerTime == nil || !sb.Status.LastTriggerTime.Time.Equal(at) {
		t.Errorf("lastTriggerTime = %v, want %v", sb.Status.LastTriggerTime, at)
	}
	if sb.Status.LastTriggerJob != jobs[0].Name {
		t.Errorf("lastTriggerJob = %q, want %q", sb.Status.LastTriggerJob, jobs[0].Name)
	}
}

func TestTriggerIsIdempotent(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), triggeredBackup(triggerAt, nil))
	for range 3 {
		reconcileBackup(t, r)
	}
	if jobs := manualJobs(t, c); len(jobs) != 1 {
		t.Fatalf("expected exactly 1 manual Job across 3 reconciles, got %d", len(jobs))
	}
}

func TestRetriggerStartsAnotherRun(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), triggeredBackup(triggerAt, nil))
	reconcileBackup(t, r)

	sb := loadBackup(t, c)
	sb.Annotations[b2backupv1.AnnotationTriggerAt] = "2026-09-03T15:30:00Z"
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	if jobs := manualJobs(t, c); len(jobs) != 2 {
		t.Fatalf("expected 2 manual Jobs after re-triggering, got %d", len(jobs))
	}
}

func TestTriggerOverridesSuspend(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(),
		triggeredBackup(triggerAt, func(sb *b2backupv1.ScheduledBackup) { sb.Spec.Suspend = true }))
	reconcileBackup(t, r)

	if jobs := manualJobs(t, c); len(jobs) != 1 {
		t.Fatalf("a suspended backup should still honour a trigger, got %d Jobs", len(jobs))
	}
}

func TestTriggerRespectsForbidConcurrency(t *testing.T) {
	sb := triggeredBackup(triggerAt, func(sb *b2backupv1.ScheduledBackup) {
		sb.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	})
	r, c := backupHarness(t, initializedRepo(), sb)

	running := &batchv1.Job{
		Namespace: testNS, Name: "already-running",
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "b2-backup-operator",
			backup.LabelTrigger:            backup.TriggerManual,
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: b2backupv1.SchemeGroupVersion.String(), Kind: "ScheduledBackup",
			Name: sb.Name, UID: sb.UID, Controller: ptr.To(true),
		}},
		Status: batchv1.JobStatus{Active: 1, StartTime: ptr.To(metav1.NewTime(time.Now()))},
	}
	if err := c.Create(context.Background(), running); err != nil {
		t.Fatal(err)
	}

	reconcileBackup(t, r)

	if jobs := manualJobs(t, c); len(jobs) != 1 {
		t.Fatalf("expected no new Job while one is running, got %d", len(jobs))
	}

	got := loadBackup(t, c)
	if got.Status.LastTriggerTime == nil {
		t.Error("a skipped trigger must still be recorded")
	}
	if got.Status.LastTriggerJob != "" {
		t.Errorf("lastTriggerJob = %q, want empty for a skipped trigger", got.Status.LastTriggerJob)
	}
}

func TestTriggerIgnoresMalformedTimestamp(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), triggeredBackup("yesterday", nil))
	reconcileBackup(t, r)

	if jobs := manualJobs(t, c); len(jobs) != 0 {
		t.Fatalf("expected no Job for a malformed trigger, got %d", len(jobs))
	}

	if loadBackup(t, c).Status.EffectiveSchedule == "" {
		t.Error("a bad annotation must not wedge the reconcile")
	}
}

func TestTriggerIsNotSkippedByItsOwnJob(t *testing.T) {
	sb := triggeredBackup(triggerAt, func(sb *b2backupv1.ScheduledBackup) {
		sb.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	})
	r, c := backupHarness(t, initializedRepo(), sb)

	reconcileBackup(t, r)

	jobs := manualJobs(t, c)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 manual Job, got %d", len(jobs))
	}

	jobs[0].Status.Active = 1
	if err := c.Status().Update(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	stale := loadBackup(t, c)
	stale.Status.LastTriggerTime = nil
	stale.Status.LastTriggerJob = ""
	if err := c.Status().Update(context.Background(), stale); err != nil {
		t.Fatal(err)
	}

	reconcileBackup(t, r)

	got := loadBackup(t, c)
	if got.Status.LastTriggerJob != jobs[0].Name {
		t.Errorf("lastTriggerJob = %q, want %q: the run was recorded as skipped",
			got.Status.LastTriggerJob, jobs[0].Name)
	}
	if n := len(manualJobs(t, c)); n != 1 {
		t.Errorf("expected no second Job, got %d", n)
	}
}

func TestARestoreHoldsTheScheduleAndTriggers(t *testing.T) {
	t.Parallel()
	for _, annotation := range []string{b2backupv1.AnnotationRestoreLock, b2backupv1.AnnotationRestorePartial} {
		t.Run(annotation, func(t *testing.T) {
			t.Parallel()
			r, c := backupHarness(t, initializedRepo(), triggeredBackup(triggerAt, func(sb *b2backupv1.ScheduledBackup) {
				sb.Annotations[annotation] = `["pvc/data:/"]`
			}))
			reconcileBackup(t, r)
			if jobs := manualJobs(t, c); len(jobs) != 0 {
				t.Errorf("started %d backups while a restore holds the backup", len(jobs))
			}
			var cj batchv1.CronJob
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
				t.Fatal(err)
			}
			if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
				t.Error("the CronJob runs while a restore holds the backup")
			}

			sb := loadBackup(t, c)
			delete(sb.Annotations, annotation)
			if err := c.Update(context.Background(), sb); err != nil {
				t.Fatal(err)
			}
			reconcileBackup(t, r)
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
				t.Fatal(err)
			}
			if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
				t.Error("the CronJob stayed suspended after the hold ended")
			}
		})
	}
}
