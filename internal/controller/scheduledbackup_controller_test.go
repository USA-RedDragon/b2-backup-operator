package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	cronJobName = resticName + "-backup"
	testRepoID  = "abc12345"

	filesBackupName = "restic-files"
	dbBackupName    = "restic-db"

	kindScheduledBackup = "ScheduledBackup"

	hourlySchedule = "@hourly"
	testScript     = "restic backup /data"
)

func backupHarness(t *testing.T, objs ...client.Object) (*ScheduledBackupReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&b2backupv1.ScheduledBackup{}, &b2backupv1.Repository{}).
		Build()
	return &ScheduledBackupReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(100),
		Config: backup.Config{
			Image: "ghcr.io/usa-reddragon/b2-backup-operator-restic:test",
		},
	}, c
}

func initializedRepo() *b2backupv1.Repository {
	return &b2backupv1.Repository{
		Name: resticName, Namespace: testNS,
		Status: b2backupv1.RepositoryStatus{Initialized: true, RepositoryID: testRepoID},
	}
}

func scheduledBackup() *b2backupv1.ScheduledBackup {
	return &b2backupv1.ScheduledBackup{
		Name: resticName, Namespace: testNS,
		Spec: b2backupv1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: resticName},
			Schedule:      hourlySchedule,
			Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
		},
	}
}

func TestUpdatesItsOwnCronJob(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	cjKey := types.NamespacedName{Namespace: testNS, Name: cronJobName}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), cjKey, &cj); err != nil {
		t.Fatalf("expected a CronJob: %v", err)
	}

	want := cj.Spec.Schedule
	cj.Spec.Schedule = "0 0 * * *"
	if err := c.Update(context.Background(), &cj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := c.Get(context.Background(), cjKey, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.Spec.Schedule != want {
		t.Errorf("drift was not corrected: schedule = %q, want %q", cj.Spec.Schedule, want)
	}
}

func TestASettledCronJobIsNotRewritten(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	cjKey := types.NamespacedName{Namespace: testNS, Name: cronJobName}
	reconcileBackup(t, r)

	var first batchv1.CronJob
	if err := c.Get(context.Background(), cjKey, &first); err != nil {
		t.Fatal(err)
	}
	first.Spec.SuccessfulJobsHistoryLimit = ptr.To(int32(3))
	first.Spec.JobTemplate.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	first.Spec.JobTemplate.Spec.Template.Spec.Containers[0].TerminationMessagePath =
		corev1.TerminationMessagePathDefault
	if err := c.Update(context.Background(), &first); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), cjKey, &first); err != nil {
		t.Fatal(err)
	}

	reconcileBackup(t, r)

	var second batchv1.CronJob
	if err := c.Get(context.Background(), cjKey, &second); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("resourceVersion %s -> %s: the CronJob is written on every pass, "+
			"which wakes every watcher of it", first.ResourceVersion, second.ResourceVersion)
	}
	if second.Spec.JobTemplate.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirst {
		t.Error("an API server default was stripped back out")
	}
}

func TestALabelledCronJobIsReadopted(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	cjKey := types.NamespacedName{Namespace: testNS, Name: cronJobName}
	reconcileBackup(t, r)

	var cj batchv1.CronJob
	if err := c.Get(context.Background(), cjKey, &cj); err != nil {
		t.Fatal(err)
	}
	cj.OwnerReferences = nil
	delete(cj.Labels, labelManagedBy)
	if err := c.Update(context.Background(), &cj); err != nil {
		t.Fatal(err)
	}

	reconcileBackup(t, r)

	if err := c.Get(context.Background(), cjKey, &cj); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&cj, loadBackup(t, c)) {
		t.Errorf("ownerReferences = %+v, want the ScheduledBackup back in control", cj.OwnerReferences)
	}
	if cj.Labels[labelManagedBy] != managedByValue {
		t.Errorf("managed-by = %q, want %q", cj.Labels[labelManagedBy], managedByValue)
	}
}

func TestClearingASpecFieldIsPushedToTheCronJob(t *testing.T) {
	sb := scheduledBackup()
	sb.Spec.TimeZone = "America/Chicago"

	r, c := backupHarness(t, initializedRepo(), sb)
	reconcileBackup(t, r)

	live := loadBackup(t, c)
	live.Spec.TimeZone = ""
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	var cj batchv1.CronJob
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.Spec.TimeZone != nil {
		t.Errorf("timeZone = %q, want it cleared with spec.timeZone", *cj.Spec.TimeZone)
	}
}

func TestRepositoryNotFoundSetsCondition(t *testing.T) {
	r, c := backupHarness(t, scheduledBackup())
	res := reconcileBackup(t, r)

	if cond := readyCondition(t, c); cond == nil || cond.Reason != "RepositoryNotFound" {
		t.Errorf("Ready condition = %+v, want reason RepositoryNotFound", cond)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 because the Repository watch recovers this", res.RequeueAfter)
	}
	assertNoCronJob(t, c)
}

func TestRepositoryNotInitializedSetsCondition(t *testing.T) {
	repo := initializedRepo()
	repo.Status.Initialized = false
	r, c := backupHarness(t, repo, scheduledBackup())
	res := reconcileBackup(t, r)

	if cond := readyCondition(t, c); cond == nil || cond.Reason != "RepositoryNotReady" {
		t.Errorf("Ready condition = %+v, want reason RepositoryNotReady", cond)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 because the Repository watch recovers this", res.RequeueAfter)
	}
	assertNoCronJob(t, c)
}

func TestCreatesCacheVolume(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	reconcileBackup(t, r)

	var pvc corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: testNS, Name: resticName + "-cache"}
	if err := c.Get(context.Background(), key, &pvc); err != nil {
		t.Fatalf("expected a cache volume: %v", err)
	}
	if owner := metav1.GetControllerOf(&pvc); owner == nil || owner.Name != resticName {
		t.Errorf("cache volume owner = %+v, want the ScheduledBackup", owner)
	}
}

func TestDisablingCacheDeletesTheVolume(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	reconcileBackup(t, r)

	key := types.NamespacedName{Namespace: testNS, Name: resticName + "-cache"}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), key, &pvc); err != nil {
		t.Fatalf("expected a cache volume: %v", err)
	}

	var sb b2backupv1.ScheduledBackup
	sbKey := types.NamespacedName{Namespace: testNS, Name: resticName}
	if err := c.Get(context.Background(), sbKey, &sb); err != nil {
		t.Fatal(err)
	}
	sb.Spec.Cache = &b2backupv1.CacheSpec{Enabled: ptr.To(false)}
	if err := c.Update(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	if err := c.Get(context.Background(), key, &pvc); !apierrors.IsNotFound(err) {
		t.Errorf("cache volume survived being disabled: %v", err)
	}
}

func TestDisablingCacheLeavesForeignClaimAlone(t *testing.T) {
	foreign := &corev1.PersistentVolumeClaim{
		Name: resticName + "-cache", Namespace: testNS,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	sb := scheduledBackup()
	sb.Spec.Cache = &b2backupv1.CacheSpec{Enabled: ptr.To(false)}

	r, c := backupHarness(t, initializedRepo(), sb, foreign)
	reconcileBackup(t, r)

	key := types.NamespacedName{Namespace: testNS, Name: resticName + "-cache"}
	if err := c.Get(context.Background(), key, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("an unowned claim was deleted: %v", err)
	}
}

func TestTerminatingBackupStopsReconciling(t *testing.T) {
	sb := scheduledBackup()
	sb.Finalizers = []string{"test.mcswain.dev/hold"}

	r, c := backupHarness(t, initializedRepo(), sb)
	reconcileBackup(t, r)

	cacheKey := types.NamespacedName{Namespace: testNS, Name: resticName + "-cache"}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), cacheKey, &pvc); err != nil {
		t.Fatalf("expected a cache volume: %v", err)
	}

	if err := c.Delete(context.Background(), loadBackup(t, c)); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &pvc); err != nil {
		t.Fatal(err)
	}

	res := reconcileBackup(t, r)
	if res != (ctrl.Result{}) {
		t.Errorf("Result = %+v, want zero while the backup is terminating", res)
	}
	if err := c.Get(context.Background(), cacheKey,
		&corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("the cache volume was recreated behind the garbage collector: %v", err)
	}
}

func TestCopiesCronJobStatusTimes(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	reconcileBackup(t, r)

	cjKey := types.NamespacedName{Namespace: testNS, Name: cronJobName}
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), cjKey, &cj); err != nil {
		t.Fatal(err)
	}
	scheduled := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	succeeded := metav1.NewTime(time.Now().Add(-30 * time.Minute).Truncate(time.Second))
	cj.Status.LastScheduleTime = &scheduled
	cj.Status.LastSuccessfulTime = &succeeded
	cj.Status.Active = []corev1.ObjectReference{{Name: "run-1"}}
	if err := c.Status().Update(context.Background(), &cj); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	var sb b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
		t.Fatal(err)
	}
	if sb.Status.LastScheduleTime == nil || !sb.Status.LastScheduleTime.Equal(&scheduled) {
		t.Errorf("lastScheduleTime = %v, want %v", sb.Status.LastScheduleTime, scheduled)
	}
	if sb.Status.LastSuccessfulTime == nil || !sb.Status.LastSuccessfulTime.Equal(&succeeded) {
		t.Errorf("lastSuccessfulTime = %v, want %v", sb.Status.LastSuccessfulTime, succeeded)
	}
	if sb.Status.Active != 1 {
		t.Errorf("active = %d, want 1", sb.Status.Active)
	}
}

func TestReadyBackupRequeuesOnTheStandardInterval(t *testing.T) {
	r, _ := backupHarness(t, initializedRepo(), scheduledBackup())

	if res := reconcileBackup(t, r); res.RequeueAfter != backupRequeueInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, backupRequeueInterval)
	}
}

func TestTwoBackupsInOneNamespaceAreBothScheduled(t *testing.T) {
	first := scheduledBackup()
	first.Name = filesBackupName
	second := scheduledBackup()
	second.Name = dbBackupName

	r, c := backupHarness(t, initializedRepo(), first, second)

	for _, name := range []string{filesBackupName, dbBackupName} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			Namespace: testNS, Name: name,
		}); err != nil {
			t.Fatalf("reconciling %s: %v", name, err)
		}
		if err := c.Get(context.Background(),
			types.NamespacedName{Namespace: testNS, Name: name + "-backup"},
			&batchv1.CronJob{}); err != nil {
			t.Errorf("%s was not scheduled: %v", name, err)
		}
	}
}

func appendOnlyRepo() *b2backupv1.Repository {
	repo := initializedRepo()
	repo.Spec.AppendOnly = true
	return repo
}

func TestAppendOnlyWithRetentionDegradesButStillSchedules(t *testing.T) {
	sb := scheduledBackup()
	sb.Spec.Retention = &b2backupv1.Retention{Hourly: ptr.To(int32(168))}

	r, c := backupHarness(t, appendOnlyRepo(), sb)
	reconcileBackup(t, r)

	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: cronJobName},
		&batchv1.CronJob{}); err != nil {
		t.Fatalf("no CronJob was created, so a working combination would stop working: %v", err)
	}
	cond := readyCondition(t, c)
	if cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != "AppendOnlyWithRetention" {
		t.Errorf("Ready = %+v, want False with reason AppendOnlyWithRetention", cond)
	}
	assertWarningEvent(t, r.Recorder, "AppendOnlyWithRetention")
}

func TestAppendOnlyWithoutRetentionStaysReady(t *testing.T) {
	r, c := backupHarness(t, appendOnlyRepo(), scheduledBackup())
	reconcileBackup(t, r)

	if cond := readyCondition(t, c); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True; no forget is rendered without spec.retention", cond)
	}
}

func TestAppendOnlyWithAScriptStaysReady(t *testing.T) {
	sb := scheduledBackup()
	sb.Spec.Sources = nil
	sb.Spec.Script = testScript
	sb.Spec.Retention = &b2backupv1.Retention{Hourly: ptr.To(int32(168))}

	r, c := backupHarness(t, appendOnlyRepo(), sb)
	reconcileBackup(t, r)

	if cond := readyCondition(t, c); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True; spec.script renders no forget", cond)
	}
}

func TestBackupsForRepositoryUsesTheIndex(t *testing.T) {
	mine := scheduledBackup()
	other := scheduledBackup()
	other.Name = "unrelated"
	other.Spec.RepositoryRef = corev1.LocalObjectReference{Name: "different-repo"}

	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mine, other).
		WithIndex(&b2backupv1.ScheduledBackup{}, repositoryRefField,
			func(o client.Object) []string {
				sb, ok := o.(*b2backupv1.ScheduledBackup)
				if !ok || sb.Spec.RepositoryRef.Name == "" {
					return nil
				}
				return []string{sb.Spec.RepositoryRef.Name}
			}).
		Build()
	r := &ScheduledBackupReconciler{Client: c, Scheme: scheme}

	got := r.backupsForRepository(context.Background(), initializedRepo())
	if len(got) != 1 || got[0].Name != resticName {
		t.Errorf("backupsForRepository() = %v, want just %s", got, resticName)
	}
}

func TestUnmanagedCronJobIsReported(t *testing.T) {
	foreign := &batchv1.CronJob{
		Name: cronJobName, Namespace: testNS,
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "c", Image: "busybox"}},
						},
					},
				},
			},
		},
	}
	r, _ := backupHarness(t, initializedRepo(), scheduledBackup())

	if err := r.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}

	r.Client = &createConflictClient{Client: r.Client}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: resticName,
	})
	if err == nil || !strings.Contains(err.Error(), "not managed by this operator") {
		t.Errorf("error = %v, want it to name the unmanaged CronJob", err)
	}
}

type createConflictClient struct {
	client.Client
}

func (c *createConflictClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if _, ok := obj.(*batchv1.CronJob); ok {
		return apierrors.NewNotFound(batchv1.Resource("cronjobs"), key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func reconcileBackup(t *testing.T, r *ScheduledBackupReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: resticName,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func assertWarningEvent(t *testing.T, recorder events.EventRecorder, reason string) {
	t.Helper()
	recorded, ok := recorder.(*events.FakeRecorder)
	if !ok {
		t.Fatalf("Recorder = %T, want the fake", recorder)
	}
	for {
		select {
		case e := <-recorded.Events:
			if strings.HasPrefix(e, corev1.EventTypeWarning+" "+reason+" ") {
				return
			}
		default:
			t.Errorf("no %s Warning event was recorded", reason)
			return
		}
	}
}

func readyCondition(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	var sb b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
		t.Fatal(err)
	}
	return apimeta.FindStatusCondition(sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady)
}

func assertNoCronJob(t *testing.T, c client.Client) {
	t.Helper()
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: cronJobName}, &batchv1.CronJob{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("a CronJob was created despite the repository not being usable: %v", err)
	}
}
