package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

func failuresAt(h int) *metav1.Time {
	return &metav1.Time{Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(h) * time.Hour)}
}

func finishedRun(name string, h int, result b2backupv1.BackupRunResult) b2backupv1.BackupRun {
	r := b2backupv1.BackupRun{JobName: name, Trigger: b2backupv1.BackupTriggerScheduled, Result: result,
		StartTime: failuresAt(h)}
	if result != b2backupv1.BackupRunRunning {
		r.CompletionTime = &metav1.Time{Time: failuresAt(h).Add(10 * time.Minute)}
	}
	return r
}

func runsNewestFirst(newest int, result b2backupv1.BackupRunResult) []b2backupv1.BackupRun {
	out := make([]b2backupv1.BackupRun, 0, historyLimit)
	for i := range historyLimit {
		out = append(out, finishedRun(fmt.Sprintf("run-%d", newest-i), newest-i, result))
	}
	return out
}

func TestRecordFailures(t *testing.T) {
	t.Parallel()
	failed, succeeded, running := b2backupv1.BackupRunFailed, b2backupv1.BackupRunSucceeded, b2backupv1.BackupRunRunning
	done := func(h int) *metav1.Time { return finishedAt(finishedRun("", h, failed)) }
	tests := []struct {
		name            string
		status          b2backupv1.ScheduledBackupStatus
		wantCount       int32
		wantLastFailure *metav1.Time
	}{
		{name: "no runs"},
		{name: "one failed run",
			status:    b2backupv1.ScheduledBackupStatus{History: []b2backupv1.BackupRun{finishedRun("a", 1, failed)}},
			wantCount: 1, wantLastFailure: done(1)},
		{name: "two failed runs in a row",
			status: b2backupv1.ScheduledBackupStatus{History: []b2backupv1.BackupRun{
				finishedRun("b", 2, failed), finishedRun("a", 1, failed)}},
			wantCount: 2, wantLastFailure: done(2)},
		{name: "a success after failures resets the count",
			status: b2backupv1.ScheduledBackupStatus{History: []b2backupv1.BackupRun{
				finishedRun("c", 3, succeeded), finishedRun("b", 2, failed), finishedRun("a", 1, failed)},
				LastSuccessfulTime: done(3)},
			wantLastFailure: done(2)},
		{name: "a failure after a success counts from the success",
			status: b2backupv1.ScheduledBackupStatus{History: []b2backupv1.BackupRun{
				finishedRun("c", 3, failed), finishedRun("b", 2, succeeded), finishedRun("a", 1, failed)},
				LastSuccessfulTime: done(2)},
			wantCount: 1, wantLastFailure: done(3)},
		{name: "a run in flight does not count",
			status: b2backupv1.ScheduledBackupStatus{History: []b2backupv1.BackupRun{
				finishedRun("c", 3, running), finishedRun("b", 2, failed), finishedRun("a", 1, failed)}},
			wantCount: 2, wantLastFailure: done(2)},
		{name: "a success the CronJob saw but the history did not",
			status: b2backupv1.ScheduledBackupStatus{History: []b2backupv1.BackupRun{
				finishedRun("b", 2, failed), finishedRun("a", 1, failed)},
				LastSuccessfulTime: done(5)},
			wantLastFailure: done(2)},
		{name: "a pass that sees the same runs again",
			status: b2backupv1.ScheduledBackupStatus{History: []b2backupv1.BackupRun{
				finishedRun("b", 2, failed), finishedRun("a", 1, failed)},
				ConsecutiveFailures: 2, LastFailureTime: done(2)},
			wantCount: 2, wantLastFailure: done(2)},
		{name: "failures that left the history are carried",
			status: b2backupv1.ScheduledBackupStatus{History: runsNewestFirst(20, failed),
				ConsecutiveFailures: 14, LastFailureTime: done(19), LastSuccessfulTime: done(1)},
			wantCount: 15, wantLastFailure: done(20)},
		{name: "a full history seen again counts nothing twice",
			status: b2backupv1.ScheduledBackupStatus{History: runsNewestFirst(20, failed),
				ConsecutiveFailures: 15, LastFailureTime: done(20)},
			wantCount: 15, wantLastFailure: done(20)},
		{name: "a full history of failures after an upgrade",
			status:    b2backupv1.ScheduledBackupStatus{History: runsNewestFirst(20, failed)},
			wantCount: historyLimit, wantLastFailure: done(20)},
		{name: "a streak recorded before the last success is not carried",
			status: b2backupv1.ScheduledBackupStatus{History: runsNewestFirst(30, failed),
				ConsecutiveFailures: 7, LastFailureTime: done(5), LastSuccessfulTime: done(10)},
			wantCount: historyLimit, wantLastFailure: done(30)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status := tt.status.DeepCopy()
			recordFailures(status)
			if status.ConsecutiveFailures != tt.wantCount {
				t.Errorf("consecutiveFailures = %d, want %d", status.ConsecutiveFailures, tt.wantCount)
			}
			if !equalTime(status.LastFailureTime, tt.wantLastFailure) {
				t.Errorf("lastFailureTime = %v, want %v", status.LastFailureTime, tt.wantLastFailure)
			}
		})
	}
}

func equalTime(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(b)
}

func backupRunJob(
	t *testing.T, c client.Client, sb *b2backupv1.ScheduledBackup, name string, h int, failed bool,
) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{
		Namespace: testNS, Name: name, UID: types.UID("uid-" + name),
		Labels: map[string]string{labelManagedBy: managedByValue, backup.LabelTrigger: backup.TriggerManual},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "b2backup.mcswain.dev/v1", Kind: kindScheduledBackup,
			Name: sb.Name, UID: sb.UID, Controller: ptr.To(true)}},
	}
	if err := c.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	end := metav1.Time{Time: failuresAt(h).Add(10 * time.Minute)}
	job.Status = batchv1.JobStatus{StartTime: failuresAt(h)}
	if failed {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			LastTransitionTime: end}}
	} else {
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &end
	}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestConsecutiveFailuresSurviveJobDeletionAndRestart(t *testing.T) {
	t.Parallel()
	sb := scheduledBackup()
	sb.UID = "uid-failing-backup"
	repo := initializedRepo()
	r, c := backupHarness(t, sb, repo)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sb)}
	restart := func() {
		r = &ScheduledBackupReconciler{Client: c, Scheme: r.Scheme, Recorder: events.NewFakeRecorder(100),
			Config: r.Config}
	}
	check := func(step string, wantCount int32, wantFailure *metav1.Time, wantReason string) {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		var got b2backupv1.ScheduledBackup
		if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.ConsecutiveFailures != wantCount {
			t.Errorf("%s: consecutiveFailures = %d, want %d", step, got.Status.ConsecutiveFailures, wantCount)
		}
		if !equalTime(got.Status.LastFailureTime, wantFailure) {
			t.Errorf("%s: lastFailureTime = %v, want %v", step, got.Status.LastFailureTime, wantFailure)
		}
		ready := apimeta.FindStatusCondition(got.Status.Conditions, b2backupv1.ScheduledBackupConditionReady)
		if ready == nil || ready.Reason != wantReason {
			t.Errorf("%s: Ready = %+v, want reason %s", step, ready, wantReason)
		}
	}
	finished := func(h int) *metav1.Time { return &metav1.Time{Time: failuresAt(h).Add(10 * time.Minute)} }

	first := backupRunJob(t, c, sb, "run-1", 1, true)
	check("one failed run", 1, finished(1), "Ready")

	if err := c.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	restart()
	check("after the Job is gone and the operator restarted", 1, finished(1), "Ready")

	backupRunJob(t, c, sb, "run-2", 2, true)
	check("a second failed run", 2, finished(2), "Ready")
	check("the same runs seen again", 2, finished(2), "Ready")

	if err := c.Delete(ctx, repo); err != nil {
		t.Fatal(err)
	}
	check("a configuration error", 2, finished(2), "RepositoryNotFound")
	if err := c.Create(ctx, initializedRepo()); err != nil {
		t.Fatal(err)
	}
	var live b2backupv1.Repository
	if err := c.Get(ctx, client.ObjectKeyFromObject(repo), &live); err != nil {
		t.Fatal(err)
	}
	live.Status = initializedRepo().Status
	if err := c.Status().Update(ctx, &live); err != nil {
		t.Fatal(err)
	}

	backupRunJob(t, c, sb, "run-3", 3, false)
	check("a run that succeeded", 0, finished(2), "Ready")

	var got b2backupv1.ScheduledBackup
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if !equalTime(got.Status.LastSuccessfulTime, finished(3)) {
		t.Errorf("lastSuccessfulTime = %v, want %v", got.Status.LastSuccessfulTime, finished(3))
	}
}

func TestAStalePassDoesNotLowerTheFailureCount(t *testing.T) {
	t.Parallel()
	failed := b2backupv1.BackupRunFailed
	sb := scheduledBackup()
	sb.Status = b2backupv1.ScheduledBackupStatus{
		History:             runsNewestFirst(12, failed),
		ConsecutiveFailures: 12,
		LastFailureTime:     finishedAt(finishedRun("", 12, failed)),
	}
	r, c := backupHarness(t, sb)

	base := scheduledBackup()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(base), base); err != nil {
		t.Fatal(err)
	}
	base.Status = b2backupv1.ScheduledBackupStatus{
		History:             runsNewestFirst(10, failed),
		ConsecutiveFailures: 10,
		LastFailureTime:     finishedAt(finishedRun("", 10, failed)),
	}
	modified := base.DeepCopy()
	modified.Status.History = mergeHistory(base.Status.History, observe(finishedRun("run-11", 11, failed)))
	recordFailures(&modified.Status)
	if modified.Status.ConsecutiveFailures != 11 {
		t.Fatalf("the stale pass counted %d, want 11", modified.Status.ConsecutiveFailures)
	}
	if err := r.patchStatus(context.Background(), modified, base); err != nil {
		t.Fatal(err)
	}

	var live b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sb), &live); err != nil {
		t.Fatal(err)
	}
	if live.Status.ConsecutiveFailures != 12 {
		t.Errorf("consecutiveFailures = %d, want the live 12", live.Status.ConsecutiveFailures)
	}
	if want := finishedAt(finishedRun("", 12, failed)); !equalTime(live.Status.LastFailureTime, want) {
		t.Errorf("lastFailureTime = %v, want %v", live.Status.LastFailureTime, want)
	}
}
