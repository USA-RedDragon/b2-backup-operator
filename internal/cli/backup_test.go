package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func trigger(t *testing.T, c client.Client, sb *b2backupv1.ScheduledBackup, wait bool) (string, error) {
	t.Helper()
	return triggerWithin(t, c, sb, wait, 2*time.Second)
}

func triggerWithin(
	t *testing.T,
	c client.Client,
	sb *b2backupv1.ScheduledBackup,
	wait bool,
	timeout time.Duration,
) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runBackupNow(context.Background(), c, fakeclientset.NewSimpleClientset(), &buf,
		sb, wait, false, timeout)
	return buf.String(), err
}

func ackTrigger(t *testing.T, c client.Client, sb *b2backupv1.ScheduledBackup, jobName string) {
	t.Helper()
	key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}
	go func() {
		ctx := context.Background()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			var latest b2backupv1.ScheduledBackup
			if err := c.Get(ctx, key, &latest); err != nil {
				return
			}
			raw := latest.Annotations[b2backupv1.AnnotationTriggerAt]
			if raw == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return
			}
			latest.Status.LastTriggerTime = &metav1.Time{Time: at}
			latest.Status.LastTriggerJob = jobName
			_ = c.Status().Update(ctx, &latest)
			return
		}
	}()
}

func TestBackupStampsTheTriggerAnnotation(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, sb)

	out, err := trigger(t, c, sb, false)
	if err != nil {
		t.Fatalf("runBackupNow: %v", err)
	}

	var got b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: testBackupName}, &got); err != nil {
		t.Fatal(err)
	}

	raw := got.Annotations[b2backupv1.AnnotationTriggerAt]
	if raw == "" {
		t.Fatal("the trigger annotation was not set")
	}

	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Errorf("annotation %q is not RFC3339: %v", raw, err)
	}
	for _, want := range []string{"triggered backup", "job/" + testBackupName + "-manual-"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestBackupWarnsWhenNotReady(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	sb.Status.Conditions = []metav1.Condition{{
		Type: b2backupv1.ScheduledBackupConditionReady, Status: metav1.ConditionFalse,
		Reason: "RepositoryNotReady", Message: "not initialized",
	}}
	c := newClient(t, sb)

	out, err := trigger(t, c, sb, false)
	if err != nil {
		t.Fatalf("runBackupNow: %v", err)
	}
	if !strings.Contains(out, "RepositoryNotReady") || !strings.HasPrefix(out, "!") {
		t.Errorf("expected a deferred-run warning:\n%s", out)
	}
}

func TestBackupReportsASkippedTrigger(t *testing.T) {
	t.Parallel()
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, sb)
	ackTrigger(t, c, sb, "")

	_, err := trigger(t, c, sb, true)
	if !errors.Is(err, ErrTriggerSkipped) {
		t.Fatalf("expected ErrTriggerSkipped, got %v", err)
	}
	if !strings.Contains(err.Error(), "Forbid") {
		t.Errorf("the error should explain why: %v", err)
	}
}

func TestBackupWaitsForAnAcknowledgedRun(t *testing.T) {
	t.Parallel()
	sb := readyBackup(testBackupName, testRepoName)

	done := jobOwnedBy(testManualJob, sb.UID, "ScheduledBackup", time.Minute)
	done.Status.Succeeded = 1
	c := newClient(t, sb, done)
	ackTrigger(t, c, sb, testManualJob)

	out, err := trigger(t, c, sb, true)
	if err != nil {
		t.Fatalf("runBackupNow: %v", err)
	}
	for _, want := range []string{"started job/web-files-manual-abc", "succeeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestBackupFailsOnAFailedRun(t *testing.T) {
	t.Parallel()
	sb := readyBackup(testBackupName, testRepoName)

	failed := jobOwnedBy(testManualJob, sb.UID, "ScheduledBackup", time.Minute)
	failed.Status.Failed = 1
	c := newClient(t, sb, failed)
	ackTrigger(t, c, sb, testManualJob)

	if _, err := trigger(t, c, sb, true); !errors.Is(err, ErrBackupFailed) {
		t.Fatalf("expected ErrBackupFailed, got %v", err)
	}
}

func TestBackupRefusesToClaimAnEarlierRun(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	ahead := metav1.NewTime(time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	sb.Status.LastTriggerTime = &ahead
	sb.Status.LastTriggerJob = testManualJob

	done := jobOwnedBy(testManualJob, sb.UID, "ScheduledBackup", time.Minute)
	done.Status.Succeeded = 1
	c := newClient(t, sb, done)

	out, err := triggerWithin(t, c, sb, true, 30*time.Second)
	if !errors.Is(err, ErrTriggerSkipped) {
		t.Fatalf("expected ErrTriggerSkipped, got %v\n%s", err, out)
	}
	if strings.Contains(out, "succeeded") {
		t.Errorf("a previous run must not be reported as this one:\n%s", out)
	}
}

func TestBackupReportsAnUnacknowledgedTrigger(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, sb)

	_, err := triggerWithin(t, c, sb, true, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	for _, want := range []string{"lastTriggerTime", "acknowledge"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in %v", want, err)
		}
	}
}

func TestBackupSurfacesAReadErrorWhileWaiting(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	c := clientBuilder(t, sb).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(
			ctx context.Context,
			cl client.WithWatch,
			key client.ObjectKey,
			obj client.Object,
			opts ...client.GetOption,
		) error {
			if _, ok := obj.(*b2backupv1.ScheduledBackup); ok {
				return errors.New("boom")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()

	if _, err := trigger(t, c, sb, true); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the read error to surface, got %v", err)
	}
}

func TestCancelDeletesRunningJobsOnly(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID

	running := jobOwnedBy("running", cj.UID, "CronJob", time.Minute)
	running.Status.Active = 1
	finished := jobOwnedBy("finished", cj.UID, "CronJob", time.Hour)
	finished.Status.Succeeded = 1

	c := newClient(t, sb, cj, running, finished)

	var buf bytes.Buffer
	if err := runCancel(context.Background(), c, &buf, sb); err != nil {
		t.Fatalf("runCancel: %v", err)
	}
	if !strings.Contains(buf.String(), "cancelled job/running") {
		t.Errorf("expected the running job to be cancelled:\n%s", buf.String())
	}

	if !strings.Contains(buf.String(), "unlock") {
		t.Errorf("expected a stale-lock warning:\n%s", buf.String())
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].Name != "finished" {
		t.Errorf("a completed job must be left alone, got %+v", jobs.Items)
	}
}

func TestCancelWithNothingRunning(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	var buf bytes.Buffer
	if err := runCancel(context.Background(), newClient(t, sb), &buf, sb); err != nil {
		t.Fatalf("runCancel: %v", err)
	}
	if !strings.Contains(buf.String(), "no backup is running") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestWaitReturnsWhenNothingIsRunning(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	var buf bytes.Buffer
	if err := runWait(context.Background(), newClient(t, sb), &buf, sb, time.Second); err != nil {
		t.Fatalf("runWait: %v", err)
	}
	if !strings.Contains(buf.String(), "no backup is running") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestWaitFailsOnAFailedRun(t *testing.T) {
	t.Parallel()
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID
	running := jobOwnedBy("running", cj.UID, "CronJob", time.Minute)
	running.Status.Active = 1
	c := newClient(t, sb, cj, running)

	go func() {
		time.Sleep(50 * time.Millisecond)
		var job batchv1.Job
		key := types.NamespacedName{Namespace: testNS, Name: "running"}
		if err := c.Get(context.Background(), key, &job); err != nil {
			return
		}
		job.Status.Active, job.Status.Failed = 0, 1
		_ = c.Status().Update(context.Background(), &job)
	}()

	var buf bytes.Buffer
	err := runWait(context.Background(), c, &buf, sb, 5*time.Second)
	if !errors.Is(err, ErrBackupFailed) {
		t.Fatalf("expected ErrBackupFailed, got %v", err)
	}
}

func TestBackupStopsWhenTheJobIsGone(t *testing.T) {
	t.Parallel()
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, sb)
	ackTrigger(t, c, sb, testManualJob)

	out, err := triggerWithin(t, c, sb, true, 30*time.Second)
	if !errors.Is(err, errJobDeleted) {
		t.Fatalf("expected errJobDeleted, got %v\n%s", err, out)
	}
	if strings.Contains(out, "succeeded") {
		t.Errorf("a job that is gone must not read as one that succeeded:\n%s", out)
	}
}

func TestWaitReportsAJobDeletedUnderIt(t *testing.T) {
	t.Parallel()
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID
	running := jobOwnedBy("running", cj.UID, "CronJob", time.Minute)
	running.Status.Active = 1
	c := newClient(t, sb, cj, running)

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = c.Delete(context.Background(), running)
	}()

	var buf bytes.Buffer
	err := runWait(context.Background(), c, &buf, sb, 30*time.Second)
	if !errors.Is(err, errJobDeleted) {
		t.Fatalf("expected errJobDeleted, got %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "finished") {
		t.Errorf("a deleted job must not be reported as finished:\n%s", buf.String())
	}
}
