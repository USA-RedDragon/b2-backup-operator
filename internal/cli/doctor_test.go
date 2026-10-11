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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

func credentialsSecret(name string) *corev1.Secret {
	ns := testNS
	return &corev1.Secret{
		Namespace: ns, Name: name,
		Data: map[string][]byte{
			controller.KeyResticRepository: []byte(testRepoURL),
			controller.KeyResticPassword:   []byte("pw"),
			controller.KeyAccessKeyID:      []byte(testKeyID),
			controller.KeySecretAccessKey:  []byte("keysecret"),
			controller.KeyDefaultRegion:    []byte("us-east-005"),
		},
	}
}

func ownedCronJob(sb *b2backupv1.ScheduledBackup) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		Namespace: sb.Namespace, Name: backup.CronJobName(sb),
	}
	cj.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: b2backupv1.SchemeGroupVersion.String(),
		Kind:       kindScheduledBackup,
		Name:       sb.Name,
		UID:        sb.UID,
		Controller: ptr.To(true),
	}}
	return cj
}

func initJobFor(r *b2backupv1.Repository) *batchv1.Job {
	job := &batchv1.Job{Namespace: r.Namespace, Name: r.Name + "-init"}
	job.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: b2backupv1.SchemeGroupVersion.String(), Kind: kindRepository,
		Name: r.Name, UID: r.UID, Controller: ptr.To(true),
	}}
	return job
}

func boundCache(sb *b2backupv1.ScheduledBackup) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Namespace: sb.Namespace, Name: backup.CacheName(sb),
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func healthy(t *testing.T) (client.Client, *b2backupv1.ScheduledBackup) {
	t.Helper()
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	sched := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	success := metav1.NewTime(time.Now().Add(-2 * time.Hour).Add(4 * time.Minute))
	sb.Status.LastScheduleTime = &sched
	sb.Status.LastSuccessfulTime = &success

	return newClient(t, r, sb, credentialsSecret(r.SecretName()),
		ownedCronJob(sb), boundCache(sb)), sb
}

func doctor(t *testing.T, c client.Client, arg string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runDoctor(context.Background(), c, &buf, "prod", arg)
	return buf.String(), err
}

func TestDoctorHealthy(t *testing.T) {
	c, _ := healthy(t)

	out, err := doctor(t, c, "sb/"+testBackupName)
	if err != nil {
		t.Fatalf("healthy backup reported unhealthy: %v\n%s", err, out)
	}
	if strings.Contains(out, "✖") {
		t.Errorf("unexpected failure in healthy output:\n%s", out)
	}
	for _, want := range []string{
		subjectBackup,
		`repository "` + testRepoName + `" is ready and initialized`,
		`CronJob "web-files-backup" is owned by this ScheduledBackup`,
		`cache PVC "web-files-cache" is bound`,
		"last backup succeeded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestDoctorDetectsCronJobAdoptionConflict(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "helm.toolkit.fluxcd.io/v2",
		Kind:       "HelmRelease",
		Name:       testBackupName,
		UID:        "uid-helmrelease",
		Controller: ptr.To(true),
	}}
	c := newClient(t, r, sb, credentialsSecret(r.SecretName()), cj, boundCache(sb))

	out, err := doctor(t, c, "sb/"+testBackupName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v", err)
	}
	for _, want := range []string{
		"is not controlled by this ScheduledBackup",
		"managed-by label",
		"HelmRelease/web-files",
		"delete cronjob/web-files-backup",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestDoctorDetectsMissingRepository(t *testing.T) {
	sb := readyBackup("orphan", "gone")
	out, err := doctor(t, newClient(t, sb), "sb/orphan")

	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v", err)
	}
	if !strings.Contains(out, `repository "gone" does not exist`) {
		t.Errorf("expected the missing repository to be named:\n%s", out)
	}
}

func TestDoctorDetectsIncompleteSecret(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	secret := credentialsSecret(r.SecretName())
	delete(secret.Data, controller.KeyResticPassword)
	c := newClient(t, r, sb, secret, ownedCronJob(sb), boundCache(sb))

	out, err := doctor(t, c, "sb/"+testBackupName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v", err)
	}
	if !strings.Contains(out, controller.KeyResticPassword) {
		t.Errorf("expected the missing key to be named:\n%s", out)
	}
}

func TestDoctorDetectsAMissingS3Key(t *testing.T) {
	r := readyRepo(testNS)
	secret := credentialsSecret(r.SecretName())
	delete(secret.Data, controller.KeySecretAccessKey)
	c := newClient(t, r, secret)

	out, err := doctor(t, c, "repo/"+testRepoName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v\n%s", err, out)
	}
	if !strings.Contains(out, controller.KeySecretAccessKey) {
		t.Errorf("expected the missing key to be named:\n%s", out)
	}
}

func TestDoctorDetectsAKeyTheOperatorDidNotRecord(t *testing.T) {
	r := readyRepo(testNS)
	secret := credentialsSecret(r.SecretName())
	secret.Data[controller.KeyAccessKeyID] = []byte("0055aa00000000000000000099")
	c := newClient(t, r, secret)

	out, err := doctor(t, c, "repo/"+testRepoName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "but the operator recorded "+testKeyID) {
		t.Errorf("expected the key mismatch to be explained:\n%s", out)
	}
}

func TestDoctorShowsWhereTheRepositoryLives(t *testing.T) {
	r := readyRepo(testNS)
	c := newClient(t, r, credentialsSecret(r.SecretName()))

	out, err := doctor(t, c, "repo/"+testRepoName)
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, want := range []string{
		"stored at " + testBucket + "/" + testPath,
		"has all 5 keys, application key " + testKeyID,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestDoctorDetectsFailedMostRecentRun(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	sched := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	success := metav1.NewTime(time.Now().Add(-49 * time.Hour))
	sb.Status.LastScheduleTime = &sched
	sb.Status.LastSuccessfulTime = &success
	c := newClient(t, r, sb, credentialsSecret(r.SecretName()), ownedCronJob(sb), boundCache(sb))

	out, err := doctor(t, c, "sb/"+testBackupName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v", err)
	}
	if !strings.Contains(out, "did not succeed") {
		t.Errorf("expected the failed run to be reported:\n%s", out)
	}
}

func TestDoctorIgnoresRunInProgress(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	sched := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	success := metav1.NewTime(time.Now().Add(-24 * time.Hour))
	sb.Status.LastScheduleTime = &sched
	sb.Status.LastSuccessfulTime = &success
	sb.Status.Active = 1
	c := newClient(t, r, sb, credentialsSecret(r.SecretName()), ownedCronJob(sb), boundCache(sb))

	out, err := doctor(t, c, "sb/"+testBackupName)
	if err != nil {
		t.Fatalf("a running backup should not fail doctor: %v\n%s", err, out)
	}
	if !strings.Contains(out, "running now") {
		t.Errorf("expected the active run to be reported:\n%s", out)
	}
}

func TestDoctorSuspendedWarnsWithoutFailing(t *testing.T) {
	c, sb := healthy(t)
	sb.Spec.Suspend = true
	if err := c.Update(context.Background(), sb); err != nil {
		t.Fatal(err)
	}

	out, err := doctor(t, c, "sb/"+testBackupName)
	if err != nil {
		t.Fatalf("a suspended backup should warn, not fail: %v\n%s", err, out)
	}
	if !strings.Contains(out, "backups are suspended") {
		t.Errorf("expected a suspension warning:\n%s", out)
	}
	if !strings.Contains(out, "CronJob suspend state does not match") {
		t.Errorf("a CronJob still firing for a suspended backup must be reported:\n%s", out)
	}
	if !strings.Contains(out, "CronJob suspended=false, spec.suspend=true") {
		t.Errorf("expected both suspend states in the detail:\n%s", out)
	}
}

func TestDoctorReportsFailedInitJob(t *testing.T) {
	r := newRepo(testNS)
	r.Status.Conditions = []metav1.Condition{{
		Type: b2backupv1.RepositoryConditionReady, Status: metav1.ConditionFalse,
		Reason: "Initializing", Message: "waiting for restic init",
	}}
	job := initJobFor(r)
	job.Status = batchv1.JobStatus{
		Failed: 3,
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Message: "Job has reached the specified backoff limit",
		}},
	}
	c := newClient(t, r, job, credentialsSecret(r.SecretName()))

	out, err := doctor(t, c, "repo/"+testRepoName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v", err)
	}
	for _, want := range []string{
		"is not initialized",
		"waiting for restic init",
		`init Job "store-init" has failed 3 time(s)`,
		"Job has reached the specified backoff limit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestDoctorChecksWholeNamespace(t *testing.T) {
	c, _ := healthy(t)
	out, err := doctor(t, c, "")
	if err != nil {
		t.Fatalf("unexpected failure: %v\n%s", err, out)
	}
	for _, want := range []string{subjectRepo, subjectBackup} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestDoctorEmptyNamespace(t *testing.T) {
	out, err := doctor(t, newClient(t), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No resources found") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestDoctorReadsHistoryForManualRuns(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	sb.Status.LastScheduleTime = nil
	sb.Status.LastSuccessfulTime = nil
	done := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	started := metav1.NewTime(done.Add(-30 * time.Second))
	sb.Status.History = []b2backupv1.BackupRun{{
		JobName: testManualJob, Trigger: b2backupv1.BackupTriggerManual,
		Result: b2backupv1.BackupRunSucceeded, StartTime: &started, CompletionTime: &done,
	}}
	c := newClient(t, r, sb, credentialsSecret(r.SecretName()), ownedCronJob(sb), boundCache(sb))

	out, err := doctor(t, c, "sb/"+testBackupName)
	if err != nil {
		t.Fatalf("a backup with a successful manual run should be healthy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "last backup succeeded") || !strings.Contains(out, "manual") {
		t.Errorf("expected the manual run to be reported:\n%s", out)
	}
	if strings.Contains(out, "no backup has run yet") {
		t.Errorf("a backup that has run must not read as one that has not:\n%s", out)
	}
}

func TestDoctorReportsAFailedRunFromHistory(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	sb.Status.LastScheduleTime = nil
	sb.Status.LastSuccessfulTime = nil
	done := metav1.NewTime(time.Now().Add(-time.Minute))
	sb.Status.History = []b2backupv1.BackupRun{{
		JobName: testManualJob, Trigger: b2backupv1.BackupTriggerManual,
		Result: b2backupv1.BackupRunFailed, CompletionTime: &done,
	}}
	c := newClient(t, r, sb, credentialsSecret(r.SecretName()), ownedCronJob(sb), boundCache(sb))

	out, err := doctor(t, c, "sb/"+testBackupName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "failed") {
		t.Errorf("expected the failure to be reported:\n%s", out)
	}
}

func TestDoctorStopsAtARepositoryConflict(t *testing.T) {
	repo := newRepo(testNS)
	repo.Status.Conditions = []metav1.Condition{{
		Type: b2backupv1.RepositoryConditionReady, Status: metav1.ConditionFalse,
		Reason:  reasonRepositoryConflict,
		Message: "bucket acme-backups path restic/prod overlaps Repository other/incumbent, which claimed it first",
	}}
	c := newClient(t, repo)

	out, err := doctor(t, c, "repo/"+testRepoName)
	if err == nil {
		t.Fatalf("a conflicted repository should fail:\n%s", out)
	}
	if !strings.Contains(out, "other/incumbent") {
		t.Errorf("expected the incumbent to be named:\n%s", out)
	}
	for _, unwanted := range []string{"not initialized", "does not exist", "no B2 bucket recorded"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("expected no %q consequence of the conflict:\n%s", unwanted, out)
		}
	}
}

func denySecretReads(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return clientBuilder(t, objs...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(
			ctx context.Context,
			c client.WithWatch,
			key client.ObjectKey,
			obj client.Object,
			opts ...client.GetOption,
		) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "secrets"}, key.Name,
					errors.New(`User "b2bk-viewer" cannot get resource "secrets"`))
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
}

func TestDoctorSkipsSecretsItMayNotRead(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	sched := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	success := metav1.NewTime(sched.Add(4 * time.Minute))
	sb.Status.LastScheduleTime = &sched
	sb.Status.LastSuccessfulTime = &success
	c := denySecretReads(t, r, sb, credentialsSecret(r.SecretName()), ownedCronJob(sb), boundCache(sb))

	out, err := doctor(t, c, "sb/"+testBackupName)
	if err != nil {
		t.Fatalf("a viewer without secrets access must not fail doctor: %v\n%s", err, out)
	}
	if strings.Contains(out, "✖") {
		t.Errorf("a forbidden read must not be a failure:\n%s", out)
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "not allowed to read it") {
		t.Errorf("expected the Secret check to be reported as skipped:\n%s", out)
	}
}

func TestDoctorExplainsAnUnfinishedInitJob(t *testing.T) {
	r := newRepo(testNS)
	c := newClient(t, r, initJobFor(r), credentialsSecret(r.SecretName()))

	out, err := doctor(t, c, "repo/"+testRepoName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v", err)
	}
	if !strings.Contains(out, `init Job "store-init"`) {
		t.Errorf("an init Job that is neither running nor failed must still be explained:\n%s", out)
	}
}

func TestDoctorExplainsAnUnreadableJobList(t *testing.T) {
	r := newRepo(testNS)
	c := clientBuilder(t, r, credentialsSecret(r.SecretName())).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				ctx context.Context,
				cl client.WithWatch,
				list client.ObjectList,
				opts ...client.ListOption,
			) error {
				if _, ok := list.(*batchv1.JobList); ok {
					return errors.New("boom")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()

	out, err := doctor(t, c, "repo/"+testRepoName)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("expected ErrUnhealthy, got %v", err)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("a swallowed List error leaves the failure unexplained:\n%s", out)
	}
}
