package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

func run(name string, minutesAgo int, result b2backupv1.BackupRunResult) b2backupv1.BackupRun {
	start := metav1.NewTime(time.Now().Add(-time.Duration(minutesAgo) * time.Minute))
	r := b2backupv1.BackupRun{
		JobName:   name,
		Trigger:   b2backupv1.BackupTriggerScheduled,
		Result:    result,
		StartTime: &start,
	}
	if result != b2backupv1.BackupRunRunning {
		done := metav1.NewTime(start.Add(4 * time.Minute))
		r.CompletionTime = &done
	}
	return r
}

func observe(runs ...b2backupv1.BackupRun) []observedRun {
	out := make([]observedRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, observedRun{run: r, active: r.Result == b2backupv1.BackupRunRunning})
	}
	return out
}

func names(runs []b2backupv1.BackupRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.JobName)
	}
	return out
}

func TestMergeHistoryOrdersNewestFirst(t *testing.T) {
	got := mergeHistory(nil, observe(
		run("old", 300, b2backupv1.BackupRunSucceeded),
		run("new", 10, b2backupv1.BackupRunSucceeded),
		run("middle", 100, b2backupv1.BackupRunFailed),
	))

	want := []string{"new", "middle", "old"}
	if fmt.Sprint(names(got)) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", names(got), want)
	}
}

func TestMergeHistoryKeepsCollectedRuns(t *testing.T) {
	existing := []b2backupv1.BackupRun{
		run("gone-2", 200, b2backupv1.BackupRunSucceeded),
		run("gone-1", 300, b2backupv1.BackupRunFailed),
	}
	got := mergeHistory(existing, observe(run("still-here", 10, b2backupv1.BackupRunSucceeded)))

	want := []string{"still-here", "gone-2", "gone-1"}
	if fmt.Sprint(names(got)) != fmt.Sprint(want) {
		t.Errorf("history = %v, want %v", names(got), want)
	}
}

func TestMergeHistoryUpdatesRunningEntryInPlace(t *testing.T) {
	existing := []b2backupv1.BackupRun{run("job-1", 10, b2backupv1.BackupRunRunning)}
	got := mergeHistory(existing, observe(run("job-1", 10, b2backupv1.BackupRunSucceeded)))

	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(got), names(got))
	}
	if got[0].Result != b2backupv1.BackupRunSucceeded {
		t.Errorf("result = %q, want Succeeded", got[0].Result)
	}
	if got[0].CompletionTime == nil {
		t.Error("completionTime should be recorded once the run finishes")
	}
}

func TestMergeHistoryKeepsATerminalResult(t *testing.T) {
	existing := []b2backupv1.BackupRun{run("job-1", 10, b2backupv1.BackupRunSucceeded)}
	got := mergeHistory(existing, observe(run("job-1", 10, b2backupv1.BackupRunRunning)))

	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(got), names(got))
	}
	if got[0].Result != b2backupv1.BackupRunSucceeded {
		t.Errorf("result = %q, want the recorded Succeeded to survive a stale read", got[0].Result)
	}
	if got[0].CompletionTime == nil {
		t.Error("completionTime was dropped with the result")
	}
}

func TestMergeHistoryIsBounded(t *testing.T) {
	existing := make([]b2backupv1.BackupRun, 0, 25)
	for i := range 25 {
		existing = append(existing, run(fmt.Sprintf("job-%02d", i), i*10, b2backupv1.BackupRunSucceeded))
	}
	got := mergeHistory(existing, nil)

	if len(got) != historyLimit {
		t.Fatalf("history length = %d, want %d", len(got), historyLimit)
	}

	if got[0].JobName != "job-00" {
		t.Errorf("newest entry = %q, want job-00", got[0].JobName)
	}
}

func TestRunFromJobClassifies(t *testing.T) {
	base := func() *batchv1.Job {
		return &batchv1.Job{
			Name: "j", CreationTimestamp: metav1.Now(),
			Status: batchv1.JobStatus{StartTime: ptr.To(metav1.Now())},
		}
	}

	succeeded := base()
	succeeded.Status.Succeeded = 1
	if got := runFromJob(succeeded); got.run.Result != b2backupv1.BackupRunSucceeded || got.active {
		t.Errorf("succeeded job = %+v", got)
	}

	failed := base()
	failed.Status.Failed = 1
	failed.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
	}}
	if got := runFromJob(failed); got.run.Result != b2backupv1.BackupRunFailed || got.active {
		t.Errorf("failed job = %+v", got)
	}

	retrying := base()
	retrying.Status.Failed = 1
	retrying.Status.Active = 1
	if got := runFromJob(retrying); got.run.Result != b2backupv1.BackupRunRunning || !got.active {
		t.Errorf("a Job retrying after a failed Pod = %+v, want Running", got)
	}

	running := base()
	running.Status.Active = 1
	if got := runFromJob(running); got.run.Result != b2backupv1.BackupRunRunning || !got.active {
		t.Errorf("running job = %+v", got)
	}

	manual := base()
	manual.Labels = map[string]string{backup.LabelTrigger: backup.TriggerManual}
	manual.Status.Succeeded = 1
	if got := runFromJob(manual); got.run.Trigger != b2backupv1.BackupTriggerManual {
		t.Errorf("trigger = %q, want Manual", got.run.Trigger)
	}
	if got := runFromJob(succeeded); got.run.Trigger != b2backupv1.BackupTriggerScheduled {
		t.Errorf("unlabelled job should read as Scheduled, got %q", got.run.Trigger)
	}
}

func TestRunFromJobRecordsWhenAFailedJobFailed(t *testing.T) {
	start := metav1.NewTime(time.Now().Add(-time.Hour))
	failedAt := metav1.NewTime(start.Add(3 * time.Second))
	job := &batchv1.Job{Status: batchv1.JobStatus{
		StartTime: &start,
		Failed:    1,
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: failedAt,
		}},
	}}

	got := runFromJob(job)
	if got.run.CompletionTime == nil || !got.run.CompletionTime.Time.Equal(failedAt.Time) {
		t.Errorf("completionTime = %v, want the Failed condition's time %v", got.run.CompletionTime, failedAt)
	}
}

func TestRunFromJobFallsBackToCreationTime(t *testing.T) {
	created := metav1.NewTime(time.Now().Add(-time.Minute))
	job := &batchv1.Job{Name: "j", CreationTimestamp: created}

	got := runFromJob(job)
	if got.run.StartTime == nil || !got.run.StartTime.Time.Equal(created.Time) {
		t.Errorf("startTime = %v, want the creation timestamp %v", got.run.StartTime, created)
	}
}

func TestHistoryRecordsATriggeredRun(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), triggeredBackup(triggerAt, nil))
	reconcileBackup(t, r)

	jobs := manualJobs(t, c)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 manual Job, got %d", len(jobs))
	}
	jobs[0].Status.Succeeded = 1
	jobs[0].Status.StartTime = ptr.To(metav1.Now())
	jobs[0].Status.CompletionTime = ptr.To(metav1.Now())
	if err := c.Status().Update(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	history := loadBackup(t, c).Status.History
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
	if history[0].JobName != jobs[0].Name {
		t.Errorf("jobName = %q, want %q", history[0].JobName, jobs[0].Name)
	}
	if history[0].Trigger != b2backupv1.BackupTriggerManual {
		t.Errorf("trigger = %q, want Manual", history[0].Trigger)
	}
	if history[0].Result != b2backupv1.BackupRunSucceeded {
		t.Errorf("result = %q, want Succeeded", history[0].Result)
	}
}

func TestHistoryIgnoresForeignJobs(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	stranger := &batchv1.Job{
		Namespace: testNS, Name: "someone-elses",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: kindCronJob,
			Name: "other", UID: "uid-stranger", Controller: ptr.To(true),
		}},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	if err := c.Create(context.Background(), stranger); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	if history := loadBackup(t, c).Status.History; len(history) != 0 {
		t.Errorf("expected no history, got %v", names(history))
	}
}
