package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const oldJobName = moreBackupName + "-backup-29000000"

func TestAJobStillRunningAnOldForgetHoldsOffTheBackupNowUsingItsTag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := scheduledBackup().EffectiveResticHost()

	sibling := scheduledBackup()
	sibling.Name = moreBackupName
	sibling.UID = "uid-more"
	sibling.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Tag: reportingTag}}
	siblingCronJob := &batchv1.CronJob{
		Namespace: testNS, Name: moreBackupName + "-backup", UID: "uid-more-cj",
		Labels: map[string]string{labelManagedBy: managedByValue},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: b2backupv1.GroupVersion.String(), Kind: kindScheduledBackup, Name: sibling.Name,
			UID: sibling.UID, Controller: ptr.To(true),
		}},
		Spec: batchv1.CronJobSpec{Schedule: "0 3 * * *", Suspend: ptr.To(false)},
	}
	siblingCronJob.Spec.JobTemplate.Annotations = map[string]string{forgetScopeAnnotation: `{"repository":"` +
		resticName + `","host":"` + host + `","tags":["` + reportingTag + `"]}`}
	oldJob := &batchv1.Job{
		Namespace: testNS, Name: oldJobName,
		Labels:      map[string]string{labelManagedBy: managedByValue},
		Annotations: map[string]string{forgetScopeAnnotation: `{"repository":"` + resticName + `","host":"` + host + `","tags":["db"]}`},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "CronJob", Name: siblingCronJob.Name, UID: siblingCronJob.UID,
			Controller: ptr.To(true),
		}},
	}
	r, c := backupHarness(t, initializedRepo(), scheduledBackup(), sibling, siblingCronJob, oldJob)

	if _, err := r.Reconcile(ctx, ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
		t.Fatal(err)
	}
	cond := readyReason(t, c, resticName)
	if cond == nil || cond.Reason != reasonTagConflict || !strings.Contains(cond.Message, oldJobName) {
		t.Fatalf("Ready = %+v, want TagConflict naming the Job whose forget still covers tag db", cond)
	}

	var job batchv1.Job
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: oldJobName}, &job); err != nil {
		t.Fatal(err)
	}
	job.Status.CompletionTime = ptr.To(metav1.NewTime(time.Now()))
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, &job); err != nil {
		if err := c.Update(ctx, &job); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{Namespace: testNS, Name: resticName}); err != nil {
		t.Fatal(err)
	}
	if cond := readyReason(t, c, resticName); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want the backup released once the old Job finished", cond)
	}
	if cronJobSuspended(t, c, cronJobName) {
		t.Error("the CronJob stayed suspended after the old Job finished")
	}
}
