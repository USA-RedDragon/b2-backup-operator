package controller

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const (
	forgetScopeAnnotation = "b2backup.mcswain.dev/forget-scope"
	goneCronJob           = "gone-backup"
)

func readyReason(t *testing.T, c client.Client, name string) *metav1.Condition {
	t.Helper()
	var sb b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &sb); err != nil {
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

func TestAnOrphanedCronJobSharingATagIsAConflict(t *testing.T) {
	t.Parallel()
	host := scheduledBackup().EffectiveResticHost()
	tests := []struct {
		name     string
		scope    string
		conflict bool
	}{
		{"same tag", `{"repository":"` + resticName + `","host":"` + host + `","tags":["db"]}`, true},
		{"rendered before scopes were recorded", "", true},
		{"another tag", `{"repository":"` + resticName + `","host":"` + host + `","tags":["other"]}`, false},
		{"another repository", `{"repository":"elsewhere","host":"` + host + `","tags":["db"]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, c := backupHarness(t, initializedRepo(), scheduledBackup(), orphanCronJob(tt.scope))
			if _, err := r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
				t.Fatal(err)
			}
			cond := readyReason(t, c, resticName)
			if got := cond != nil && cond.Reason == reasonTagConflict; got != tt.conflict {
				t.Fatalf("Ready = %+v, want conflict %t", cond, tt.conflict)
			}
			if !tt.conflict {
				return
			}
			if !strings.Contains(cond.Message, goneCronJob) {
				t.Errorf("the conflict does not name the orphaned CronJob: %s", cond.Message)
			}
			if !cronJobSuspended(t, c, goneCronJob) {
				t.Error("the orphaned CronJob, which nothing else reconciles, kept forgetting under the shared tag")
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
