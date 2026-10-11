package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func jobOwnedBy(name string, ownerUID types.UID, ownerKind string, startedAgo time.Duration) *batchv1.Job {
	start := metav1.NewTime(time.Now().Add(-startedAgo))
	job := &batchv1.Job{
		Namespace: testNS, Name: name, UID: types.UID("uid-" + name),
		CreationTimestamp: start,
		Status:            batchv1.JobStatus{StartTime: &start},
	}
	job.OwnerReferences = []metav1.OwnerReference{{
		Kind: ownerKind, Name: "owner", UID: ownerUID, Controller: ptr.To(true),
	}}
	return job
}

func jobPod(name, jobName string) *corev1.Pod {
	return &corev1.Pod{
		Namespace: testNS, Name: name,
		Labels: map[string]string{"batch.kubernetes.io/job-name": jobName},
	}
}

func TestBackupJobsFindsBothOwnersNewestFirst(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID

	c := newClient(t, sb, cj,
		jobOwnedBy("scheduled-old", cj.UID, "CronJob", 48*time.Hour),
		jobOwnedBy("scheduled-new", cj.UID, "CronJob", 2*time.Hour),

		jobOwnedBy("manual", sb.UID, "ScheduledBackup", 30*time.Minute),

		jobOwnedBy("someone-elses", "uid-stranger", "CronJob", 1*time.Minute),
	)

	jobs, err := BackupJobs(context.Background(), c, sb)
	if err != nil {
		t.Fatalf("BackupJobs: %v", err)
	}

	names := make([]string, 0, len(jobs))
	for i := range jobs {
		names = append(names, jobs[i].Name)
	}
	want := []string{"manual", "scheduled-new", "scheduled-old"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("jobs = %v, want %v", names, want)
	}
}

func TestBackupJobsWithoutCronJob(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, sb, jobOwnedBy("manual", sb.UID, "ScheduledBackup", time.Minute))

	jobs, err := BackupJobs(context.Background(), c, sb)
	if err != nil {
		t.Fatalf("a missing CronJob must not be an error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Name != "manual" {
		t.Errorf("unexpected jobs: %+v", jobs)
	}
}

func TestSelectJob(t *testing.T) {
	jobs := []batchv1.Job{
		*jobOwnedBy("new", "u", "CronJob", time.Hour),
		*jobOwnedBy("old", "u", "CronJob", 2*time.Hour),
	}

	got, err := selectJob(jobs, false)
	if err != nil || got.Name != "new" {
		t.Errorf("selectJob(latest) = %v, %v", got, err)
	}
	got, err = selectJob(jobs, true)
	if err != nil || got.Name != "old" {
		t.Errorf("selectJob(previous) = %v, %v", got, err)
	}

	if _, err := selectJob(nil, false); !errors.Is(err, ErrNoRuns) {
		t.Errorf("expected ErrNoRuns, got %v", err)
	}

	_, err = selectJob(jobs[:1], true)
	if !errors.Is(err, ErrNoRuns) || !strings.Contains(err.Error(), "only one run") {
		t.Errorf("expected a single-run message, got %v", err)
	}
}

func TestRunLogsStreamsMostRecent(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID
	c := newClient(t, sb, cj,
		jobOwnedBy("scheduled-old", cj.UID, "CronJob", 48*time.Hour),
		jobOwnedBy("scheduled-new", cj.UID, "CronJob", 2*time.Hour),
		jobPod("scheduled-new-abcde", "scheduled-new"),
		jobPod("scheduled-old-zyxwv", "scheduled-old"),
	)

	var buf bytes.Buffer
	err := runLogs(context.Background(), c, fakeclientset.NewSimpleClientset(), &buf,
		sb, false, corev1.PodLogOptions{})
	if err != nil {
		t.Fatalf("runLogs: %v", err)
	}
	if !strings.Contains(buf.String(), "# job/scheduled-new") {
		t.Errorf("expected the newest job to be chosen:\n%s", buf.String())
	}
}

func TestRunLogsNoPods(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID
	c := newClient(t, sb, cj, jobOwnedBy("scheduled", cj.UID, "CronJob", time.Hour))

	var buf bytes.Buffer
	err := runLogs(context.Background(), c, fakeclientset.NewSimpleClientset(), &buf,
		sb, false, corev1.PodLogOptions{})
	if !errors.Is(err, ErrNoRuns) {
		t.Fatalf("expected ErrNoRuns, got %v", err)
	}
	if !strings.Contains(err.Error(), "garbage collected") {
		t.Errorf("error should explain why: %v", err)
	}
}

func TestRunLogsNoRuns(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	var buf bytes.Buffer
	err := runLogs(context.Background(), newClient(t, sb), fakeclientset.NewSimpleClientset(), &buf,
		sb, false, corev1.PodLogOptions{})
	if !errors.Is(err, ErrNoRuns) {
		t.Fatalf("expected ErrNoRuns, got %v", err)
	}
	if !strings.Contains(err.Error(), subjectBackup) {
		t.Errorf("error should name the backup: %v", err)
	}
}

func TestPodsForJobFallsBackToLegacyLabel(t *testing.T) {
	job := jobOwnedBy("scheduled", "u", "CronJob", time.Hour)
	pod := jobPod("scheduled-abcde", "scheduled")
	pod.Labels = map[string]string{"job-name": "scheduled"}

	pods, err := podsForJob(context.Background(), newClient(t, job, pod), job)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 {
		t.Fatalf("expected the legacy label to match, got %d pods", len(pods))
	}
}
