package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	forgetScopeAnnotation = "b2backup.mcswain.dev/forget-scope"
	goneCronJob           = "gone-backup"
	goneBackup            = "gone-sb"
)

func readyReason(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	var sb b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
		t.Fatal(err)
	}
	return apimeta.FindStatusCondition(sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady)
}

func cronJobSuspended(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &cj); err != nil {
		t.Fatal(err)
	}
	return cj.Spec.Suspend != nil && *cj.Spec.Suspend
}

func TestAnOrphanedCronJobOfItsNameIsSuspendedOnFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	reconcileBackup(t, r)

	var cj batchv1.CronJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
		t.Fatal(err)
	}
	cj.OwnerReferences = nil
	if err := c.Update(ctx, &cj); err != nil {
		t.Fatal(err)
	}
	var sb b2backupv1.ScheduledBackup
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
		t.Fatal(err)
	}
	sb.Spec.RepositoryRef.Name = "not-yet"
	if err := c.Update(ctx, &sb); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
		t.Fatal(err)
	}
	if !cronJobSuspended(t, c, cronJobName) {
		t.Error("a CronJob orphaned with --cascade=orphan kept firing under a spec that no longer renders")
	}
}

func orphanCronJob(scope string) *batchv1.CronJob {
	cj := &batchv1.CronJob{
		Namespace: testNS, Name: goneCronJob,
		Labels: map[string]string{labelManagedBy: managedByValue, "app.kubernetes.io/name": "gone"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 3 * * *",
			Suspend:  ptr.To(false),
		},
	}
	if scope != "" {
		cj.Spec.JobTemplate.Annotations = map[string]string{forgetScopeAnnotation: scope}
	}
	return cj
}

func TestAnOrphanedCronJobIsSuspendedWhateverItsScope(t *testing.T) {
	t.Parallel()
	host := scheduledBackup().EffectiveResticHost()
	for name, scope := range map[string]string{
		"same tag":                             `{"repository":"` + resticName + `","host":"` + host + `","tags":["db"]}`,
		"rendered before scopes were recorded": "",
		"another tag":                          `{"repository":"` + resticName + `","host":"` + host + `","tags":["other"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, c := backupHarness(t, initializedRepo(), scheduledBackup(), orphanCronJob(scope))
			if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
				t.Fatal(err)
			}
			if !cronJobSuspended(t, c, goneCronJob) {
				t.Error("the orphaned CronJob, which nothing else reconciles, kept backing up and forgetting")
			}
			if cond := readyReason(t, c); cond == nil || cond.Status != metav1.ConditionTrue {
				t.Errorf("Ready = %+v; a suspended stray forgets nothing and should not hold this backup", cond)
			}
		})
	}
}

func TestBackupJobsRecordTheirForgetScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	reconcileBackup(t, r)
	var cj batchv1.CronJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
		t.Fatal(err)
	}
	host := scheduledBackup().EffectiveResticHost()
	want := `{"repository":"` + resticName + `","host":"` + host + `","tags":["db"]}`
	if got := cj.Spec.JobTemplate.Annotations[forgetScopeAnnotation]; got != want {
		t.Errorf("job template scope = %q, want %q", got, want)
	}
}

func TestAnOrphanedCronJobNoBackupCanAdoptIsSuspended(t *testing.T) {
	t.Parallel()
	scope := `{"repository":"elsewhere","host":"` + scheduledBackup().EffectiveResticHost() + `","tags":["db"]}`
	r, c := backupHarness(t, initializedRepo(), scheduledBackup(), orphanCronJob(scope))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
		t.Fatal(err)
	}
	if cond := readyReason(t, c); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v; a stray CronJob on another Repository is no conflict", cond)
	}
	if !cronJobSuspended(t, c, goneCronJob) {
		t.Error("a CronJob whose ScheduledBackup is gone kept backing up and forgetting under a spec that no longer exists")
	}
}

func TestAStrayIsSuspendedWhenItsScheduledBackupIsGone(t *testing.T) {
	t.Parallel()
	stray := orphanCronJob("")
	stray.Name = goneBackup + backup.CronJobSuffix
	r, c := backupHarness(t, stray)
	if got := r.cronJobRequests(context.Background(), stray); len(got) != 1 || got[0].Name != goneBackup {
		t.Fatalf("requests = %v, want one for the ScheduledBackup the CronJob is named for", got)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: goneBackup}); err != nil {
		t.Fatal(err)
	}
	if !cronJobSuspended(t, c, stray.Name) {
		t.Error("the CronJob of the only ScheduledBackup, orphaned, kept running")
	}
}

func TestAStrayIsSuspendedWhileTheOtherBackupWaitsForItsRepository(t *testing.T) {
	t.Parallel()
	repo := initializedRepo()
	repo.Status.Initialized = false
	r, c := backupHarness(t, repo, scheduledBackup(), orphanCronJob(""))
	if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
		t.Fatal(err)
	}
	if !cronJobSuspended(t, c, goneCronJob) {
		t.Error("a stray kept running because the other ScheduledBackup stopped before the conflict check")
	}
}

func TestAStrayThatCannotBeSuspendedDoesNotStopTheOtherBackups(t *testing.T) {
	t.Parallel()
	errDenied := errors.New("admission webhook denied the request")
	elsewhere := `{"repository":"elsewhere","host":"` + scheduledBackup().EffectiveResticHost() + `","tags":["db"]}`
	r, c := backupHarness(t, initializedRepo(), scheduledBackup(), orphanCronJob(elsewhere))
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{Patch: func(
		ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption,
	) error {
		if obj.GetName() == goneCronJob {
			return errDenied
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
		t.Fatalf("err = %v; a stray this backup does not own failed its reconcile", err)
	}
	if cronJobSuspended(t, c, cronJobName) {
		t.Error("the backup's own CronJob was suspended because another CronJob could not be")
	}
}
