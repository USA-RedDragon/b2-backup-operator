package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func status(t *testing.T, c client.Client, arg string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := runStatus(context.Background(), c, &buf, "prod", arg, defaultRunLimit); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	return buf.String()
}

func TestStatusBackup(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = "uid-cronjob"

	good := jobOwnedBy("web-files-backup-2", cj.UID, "CronJob", 2*time.Hour)
	done := metav1.NewTime(good.Status.StartTime.Add(4*time.Minute + 12*time.Second))
	good.Status.CompletionTime = &done
	good.Status.Succeeded = 1

	bad := jobOwnedBy("web-files-backup-1", cj.UID, "CronJob", 26*time.Hour)
	badDone := metav1.NewTime(bad.Status.StartTime.Add(18 * time.Second))
	bad.Status.CompletionTime = &badDone
	bad.Status.Failed = 1

	out := status(t, newClient(t, r, sb, cj, good, bad), "sb/web-files")

	for _, want := range []string{
		subjectBackup,
		"Repository", testRepoName, testUsage + " / " + testQuota,
		"Schedule", testSchedule, testTimeZone,
		"CONDITION", "Ready", statusTrue,
		"RESULT", "STARTED", "DURATION", "JOB",
		"succeeded", "4m12s", "web-files-backup-2",
		"failed", "18s", "web-files-backup-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}

	if strings.Index(out, "web-files-backup-2") > strings.Index(out, "web-files-backup-1") {
		t.Errorf("runs are not newest-first:\n%s", out)
	}
}

func TestStatusRepository(t *testing.T) {
	out := status(t, newClient(t, readyRepo(testNS)), "repo/"+testRepoName)
	for _, want := range []string{
		subjectRepo, "BorgBase ID", testRepoID,
		testServer, testUsage + " / " + testQuota, "Initialized", statusTrue,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestStatusNoRuns(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	out := status(t, newClient(t, r, sb), "sb/web-files")
	if !strings.Contains(out, "No runs are still present") {
		t.Errorf("expected the empty-runs notice:\n%s", out)
	}
}

func TestStatusToleratesMissingRepository(t *testing.T) {
	sb := readyBackup("orphan", "gone")
	out := status(t, newClient(t, sb), "sb/orphan")
	if !strings.Contains(out, "unreadable") {
		t.Errorf("expected the repository error inline:\n%s", out)
	}
	if !strings.Contains(out, "Schedule") {
		t.Errorf("the rest of the status should still render:\n%s", out)
	}
}

func TestRunResultAndDuration(t *testing.T) {
	running := jobOwnedBy("j", "u", "CronJob", time.Minute)
	running.Status.Active = 1
	if got := runResult(running); got != "running" {
		t.Errorf("runResult(active) = %q", got)
	}

	pending := jobOwnedBy("j", "u", "CronJob", time.Minute)
	pending.Status.StartTime = nil
	if got := runDuration(pending); got != "-" {
		t.Errorf("runDuration(no start) = %q, want -", got)
	}

	failed := jobOwnedBy("j", "u", "CronJob", 10*time.Hour)
	failed.Status.Failed = 1
	failed.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobFailed,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(failed.Status.StartTime.Add(30 * time.Second)),
	}}
	if got := runDuration(failed); got != "30s" {
		t.Errorf("runDuration(failed) = %q, want 30s", got)
	}
}

func TestUsageOf(t *testing.T) {
	r := newRepo(testNS)
	if got := usageOf(r); got != "<unknown>" {
		t.Errorf("usageOf(empty) = %q", got)
	}
	r.Status.CurrentUsage = testUsage
	if got := usageOf(r); got != testUsage {
		t.Errorf("usageOf(no quota) = %q", got)
	}
	r.Status.Quota = testQuota
	if got := usageOf(r); got != testUsage+" / "+testQuota {
		t.Errorf("usageOf(both) = %q", got)
	}
}

func TestStatusDurationUsesTheTrueFailureCondition(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID

	job := jobOwnedBy("web-files-backup-1", cj.UID, "CronJob", time.Hour)
	job.Status.Failed = 1
	failedAt := metav1.NewTime(job.Status.StartTime.Add(30 * time.Second))
	stale := metav1.NewTime(job.Status.StartTime.Add(9 * time.Minute))
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: failedAt},
		{Type: batchv1.JobFailed, Status: corev1.ConditionFalse, LastTransitionTime: stale},
	}

	out := status(t, newClient(t, r, sb, cj, job), "sb/"+testBackupName)
	if !strings.Contains(out, "30s") {
		t.Errorf("expected the duration to come from the true JobFailed condition:\n%s", out)
	}
	if strings.Contains(out, "9m0s") {
		t.Errorf("a JobFailed condition with status False must be ignored:\n%s", out)
	}
}
