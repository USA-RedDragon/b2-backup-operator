package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	cronJobName = resticName + "-backup"
	testRepoID  = "abc12345"

	filesBackupName = "restic-files"
	dbBackupName    = "restic-db"

	kindScheduledBackup = "ScheduledBackup"
	kindCronJob         = "CronJob"
	moreBackupName      = "restic-more"
	elsewhereBackupName = "restic-elsewhere"
	reportingTag        = "db-reporting"

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
			Image: "ghcr.io/usa-reddragon/restic:test",
		},
	}, c
}

func initializedRepo() *b2backupv1.Repository {
	return &b2backupv1.Repository{
		Name: resticName, Namespace: testNS,
		Status: b2backupv1.RepositoryStatus{Initialized: true, BucketID: "bucket0001", BucketName: ownBucket},
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
	cj.Spec.Schedule = "1 2 * * *"
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
	second.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: "db-second"}}

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

func TestAnUnownedCronJobIsNeverTakenOver(t *testing.T) {
	t.Parallel()
	foreign := &batchv1.CronJob{
		Name: cronJobName, Namespace: testNS,
		Spec: batchv1.CronJobSpec{
			Schedule: "5 4 * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "someone/else"}}},
			}}},
		},
	}
	r, c := backupHarness(t, initializedRepo(), scheduledBackup(), foreign)
	reconcileBackup(t, r)

	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.Spec.Schedule != "5 4 * * *" || cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image != "someone/else" ||
		len(cj.OwnerReferences) != 0 {
		t.Errorf("an unowned CronJob was rewritten: %+v", cj.Spec)
	}
	var sb b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
		t.Fatal(err)
	}
	if cond := apimeta.FindStatusCondition(sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "CronJobNotManaged" {
		t.Errorf("Ready = %+v, want False CronJobNotManaged", cond)
	}
}

func TestTwoBackupsIntoOneRepositoryNeedTagsOfTheirOwn(t *testing.T) {
	t.Parallel()
	older := scheduledBackup()
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	newer := scheduledBackup()
	newer.Name = moreBackupName
	newer.CreationTimestamp = metav1.NewTime(time.Now())
	r, c := backupHarness(t, initializedRepo(), older, newer)
	for _, name := range []string{resticName, moreBackupName} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	var got b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: moreBackupName}, &got); err != nil {
		t.Fatal(err)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, b2backupv1.ScheduledBackupConditionReady); cond == nil ||
		cond.Reason != reasonTagConflict || !strings.Contains(cond.Message, `"db"`) {
		t.Errorf("Ready = %+v, want TagConflict naming tag db", cond)
	}
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: moreBackupName + "-backup"}, &cj); !apierrors.IsNotFound(err) {
		t.Errorf("the newer backup got a CronJob that would expire the older one's snapshots: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
		t.Fatal(err)
	}
	switch err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); {
	case apierrors.IsNotFound(err):
	case err != nil:
		t.Fatal(err)
	case cj.Spec.Suspend == nil || !*cj.Spec.Suspend:
		t.Error("the older backup's CronJob kept running beside a conflict")
	}

	newer = &got
	newer.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: reportingTag}}
	if err := c.Update(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: moreBackupName}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: moreBackupName + "-backup"}, &cj); err != nil {
		t.Errorf("a backup with tags of its own got no CronJob: %v", err)
	}
}

func TestAScriptForgettingEveryTagConflicts(t *testing.T) {
	t.Parallel()
	script := scheduledBackup()
	script.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	script.Spec.Sources = nil
	script.Spec.Script = "restic backup --tag=db --stdin-from-command -- backup-cnpg.sh\nrestic forget --prune \\\n  --keep-last=1"
	sources := scheduledBackup()
	sources.Name = moreBackupName
	sources.CreationTimestamp = metav1.NewTime(time.Now())
	sources.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: reportingTag}}
	elsewhere := scheduledBackup()
	elsewhere.Name = elsewhereBackupName
	elsewhere.CreationTimestamp = metav1.NewTime(time.Now())
	elsewhere.Spec.ResticHost = "another-host"
	elsewhere.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: "db-other"}}
	r, c := backupHarness(t, initializedRepo(), script, sources, elsewhere)
	for _, name := range []string{resticName, moreBackupName, elsewhereBackupName} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{resticName, moreBackupName, elsewhereBackupName} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name     string
		conflict bool
	}{
		{resticName, true},
		{moreBackupName, true},
		{elsewhereBackupName, true},
	}
	for _, tt := range tests {
		var got b2backupv1.ScheduledBackup
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: tt.name}, &got); err != nil {
			t.Fatal(err)
		}
		cond := apimeta.FindStatusCondition(got.Status.Conditions, b2backupv1.ScheduledBackupConditionReady)
		if (cond != nil && cond.Reason == reasonTagConflict) != tt.conflict {
			t.Errorf("%s: Ready = %+v, want conflict %t", tt.name, cond, tt.conflict)
		}
	}
}

func TestAScriptBackupConflictsWithAnyOther(t *testing.T) {
	t.Parallel()
	scripts := []string{
		"restic forget --tag ni'ghtly' --keep-last 1",
		`restic forget --tag mine --tag "nigh"tly --keep-last 1`,
		"restic forg''et --keep-last 1",
		"sh -c 'restic forget --keep-last 1'",
		"R=restic; $R forget --keep-last 1",
		`r() { restic "$@"; }; r forget --keep-last 1`,
		"echo forget --keep-last 1 | xargs restic",
		"restic backup --tag=mine /data\nrestic forget --prune --tag=mine --keep-last=1",
	}
	for i, script := range scripts {
		for _, scriptFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d-%t", i, scriptFirst), func(t *testing.T) {
				t.Parallel()
				older, newer := time.Now().Add(-time.Hour), time.Now()
				withScript := scheduledBackup()
				withScript.Spec.Sources = nil
				withScript.Spec.Script = script
				withSources := scheduledBackup()
				withSources.Name = moreBackupName
				withSources.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: "nightly"}}
				withScript.CreationTimestamp, withSources.CreationTimestamp = metav1.NewTime(newer), metav1.NewTime(older)
				loser := withScript.Name
				if scriptFirst {
					withScript.CreationTimestamp, withSources.CreationTimestamp = metav1.NewTime(older), metav1.NewTime(newer)
					loser = withSources.Name
				}
				r, c := backupHarness(t, initializedRepo(), withScript, withSources)
				if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: loser}); err != nil {
					t.Fatal(err)
				}
				var got b2backupv1.ScheduledBackup
				if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: loser}, &got); err != nil {
					t.Fatal(err)
				}
				if cond := apimeta.FindStatusCondition(got.Status.Conditions, b2backupv1.ScheduledBackupConditionReady); cond == nil ||
					cond.Reason != reasonTagConflict {
					t.Errorf("Ready = %+v, want TagConflict beside a script that can forget any tag", cond)
				}
			})
		}
	}
}

func TestEditingTheOlderBackupIntoAConflictStopsIt(t *testing.T) {
	t.Parallel()
	older := scheduledBackup()
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	older.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: "old-own"}}
	newer := scheduledBackup()
	newer.Name = moreBackupName
	newer.CreationTimestamp = metav1.NewTime(time.Now())
	newer.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: reportingTag}}
	r, c := backupHarness(t, initializedRepo(), older, newer)
	reconcile := func(name string) {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile(resticName)
	reconcile(moreBackupName)

	var edited b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: resticName}, &edited); err != nil {
		t.Fatal(err)
	}
	edited.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: reportingTag}}
	if err := c.Update(context.Background(), &edited); err != nil {
		t.Fatal(err)
	}
	reconcile(resticName)

	var got b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: resticName}, &got); err != nil {
		t.Fatal(err)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, b2backupv1.ScheduledBackupConditionReady); cond == nil ||
		cond.Reason != reasonTagConflict {
		t.Errorf("Ready = %+v, want the edited older backup stopped with TagConflict", cond)
	}
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil ||
		cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Errorf("the edited older backup's CronJob still runs a forget over the newer one's tag: %v", err)
	}
	if got := r.siblingBackups(context.Background(), &edited); len(got) != 1 || got[0].Name != moreBackupName {
		t.Errorf("siblingBackups = %v, want the other backup re-checked", got)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: resticName}, &edited); err != nil {
		t.Fatal(err)
	}
	edited.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: "old-own"}}
	if err := c.Update(context.Background(), &edited); err != nil {
		t.Fatal(err)
	}
	reconcile(resticName)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil ||
		cj.Spec.Suspend == nil || *cj.Spec.Suspend {
		t.Errorf("the CronJob stayed suspended after the conflict was resolved: %v", err)
	}
}

func TestACronJobThatCannotBeRebuiltIsSuspended(t *testing.T) {
	t.Parallel()
	rejectMars := interceptor.Funcs{Update: func(
		ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption,
	) error {
		if cj, ok := obj.(*batchv1.CronJob); ok && cj.Spec.TimeZone != nil && *cj.Spec.TimeZone == "Mars/Olympus" {
			return apierrors.NewInvalid(schema.GroupKind{Group: "batch", Kind: kindCronJob}, cj.Name,
				field.ErrorList{field.Invalid(field.NewPath("spec", "timeZone"), *cj.Spec.TimeZone, "unknown time zone")})
		}
		return cl.Update(ctx, obj, opts...)
	}}
	tests := []struct {
		name  string
		edit  func(*b2backupv1.ScheduledBackup)
		funcs interceptor.Funcs
	}{
		{"moved to a Repository that does not exist", func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.RepositoryRef.Name = "not-yet"
		}, interceptor.Funcs{}},
		{"a spec the operator rejects", func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Env = map[string]string{"RESTIC_HOST": "elsewhere"}
		}, interceptor.Funcs{}},
		{"a cache size that does not parse", func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Cache = &b2backupv1.CacheSpec{Size: "lots"}
		}, interceptor.Funcs{}},
		{"a CronJob update the API server rejects", func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.TimeZone = "Mars/Olympus"
		}, rejectMars},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r, c := backupHarness(t, initializedRepo(), scheduledBackup())
			r.Client = interceptor.NewClient(c.(client.WithWatch), tt.funcs)
			reconcileBackup(t, r)
			var sb b2backupv1.ScheduledBackup
			if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
				t.Fatal(err)
			}
			original := sb.Spec.DeepCopy()
			tt.edit(&sb)
			if err := c.Update(ctx, &sb); err != nil {
				t.Fatal(err)
			}
			_, _ = r.Reconcile(ctx, ctrl.Request{Namespace: testNS, Name: resticName})
			var cj batchv1.CronJob
			if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
				t.Fatal(err)
			}
			if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
				t.Fatal("a CronJob from a spec that no longer exists kept firing")
			}

			if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
				t.Fatal(err)
			}
			sb.Spec = *original
			if err := c.Update(ctx, &sb); err != nil {
				t.Fatal(err)
			}
			reconcileBackup(t, r)
			if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
				t.Fatal(err)
			}
			if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
				t.Error("the CronJob stayed suspended after the spec was fixed")
			}
		})
	}
}

func TestTheCronJobRecordsTheGenerationsItWasRenderedFrom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := initializedRepo()
	repo.Generation = 4
	sb := scheduledBackup()
	sb.Generation = 7
	r, c := backupHarness(t, repo, sb)
	reconcileBackup(t, r)
	rendered := func() string {
		t.Helper()
		var cj batchv1.CronJob
		if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
			t.Fatal(err)
		}
		return cj.Annotations["b2backup.mcswain.dev/rendered-from"]
	}
	if got := rendered(); got != "scheduledbackup=7,repository=4" {
		t.Fatalf("rendered-from = %q", got)
	}

	var latest b2backupv1.Repository
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: resticName}, &latest); err != nil {
		t.Fatal(err)
	}
	latest.Generation = 5
	if err := c.Update(ctx, &latest); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)
	if got := rendered(); got != "scheduledbackup=7,repository=5" {
		t.Errorf("rendered-from = %q after a Repository change that renders the same CronJob", got)
	}
}

func TestABackupStopsWhileItsRepositoryIsNotItsOwn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		edit    func(*b2backupv1.Repository)
		suspend bool
	}{
		{"a Repository in conflict", repoReadyFalse(reasonConflict), true},
		{"a Secret holding a retained password", repoReadyFalse(reasonRetainedPassword), true},
		{"credentials for another account", repoReadyFalse(reasonAccountMismatch), true},
		{"a Secret the operator does not manage", repoReadyFalse(reasonSecretNotManaged), true},
		{"a secretName unlike the Secret status records", func(r *b2backupv1.Repository) {
			r.Spec.SecretName = "other-credentials"
		}, true},
		{"a transient B2 failure", repoReadyFalse("ReconcileFailed"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repo := initializedRepo()
			repo.Status.SecretName = credsSecretName
			r, c := backupHarness(t, repo, scheduledBackup())
			reconcileBackup(t, r)

			var live b2backupv1.Repository
			if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: resticName}, &live); err != nil {
				t.Fatal(err)
			}
			tt.edit(&live)
			if err := c.Status().Update(ctx, &live); err != nil {
				t.Fatal(err)
			}
			tt.edit(&live)
			if err := c.Update(ctx, &live); err != nil {
				t.Fatal(err)
			}
			_, _ = r.Reconcile(ctx, ctrl.Request{Namespace: testNS, Name: resticName})

			var cj batchv1.CronJob
			if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
				t.Fatal(err)
			}
			suspended := cj.Spec.Suspend != nil && *cj.Spec.Suspend
			if suspended != tt.suspend {
				t.Errorf("suspended = %t, want %t", suspended, tt.suspend)
			}
			if cond := readyCondition(t, c); tt.suspend && (cond == nil || cond.Reason != reasonRepositoryNotOwn) {
				t.Errorf("Ready = %+v, want %s", cond, reasonRepositoryNotOwn)
			}
		})
	}
}

func repoReadyFalse(reason string) func(*b2backupv1.Repository) {
	return func(r *b2backupv1.Repository) {
		apimeta.SetStatusCondition(&r.Status.Conditions, metav1.Condition{
			Type: b2backupv1.RepositoryConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: reason,
		})
	}
}
