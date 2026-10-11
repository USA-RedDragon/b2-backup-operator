package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

// ErrRestoreRunning means another restore that overwrites live data holds the
// ScheduledBackup.
var ErrRestoreRunning = errors.New("another restore is running")

const restoreResumeTrue = "true"

// restoreLock is a ScheduledBackup held by one overwriting restore: the
// AnnotationRestoreLock annotation names it, and the schedule is suspended.
type restoreLock struct {
	c   client.Client
	key types.NamespacedName
	id  string

	// suspended is true when the schedule is to be resumed once a restore
	// succeeds: this restore suspended it, or an earlier one that failed did.
	suspended bool
}

func restoreLockID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("b2bk pid %d on %s at %s", os.Getpid(), host, time.Now().UTC().Format(time.RFC3339))
}

// acquireRestoreLock takes the ScheduledBackup for one overwriting restore and
// suspends its schedule. The write carries the resourceVersion it read, so of
// two restores racing for it only one wins.
func acquireRestoreLock(
	ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup,
) (*restoreLock, error) {
	key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}
	var latest b2backupv1.ScheduledBackup
	if err := c.Get(ctx, key, &latest); err != nil {
		return nil, err
	}
	if holder := latest.Annotations[b2backupv1.AnnotationRestoreLock]; holder != "" {
		return nil, fmt.Errorf("%w: scheduledbackup/%s is held by %s; if that restore is gone, release it with "+
			"kubectl annotate -n %s scheduledbackup/%s %s-", ErrRestoreRunning, sb.Name, holder,
			sb.Namespace, sb.Name, b2backupv1.AnnotationRestoreLock)
	}

	lock := &restoreLock{c: c, key: key, id: restoreLockID()}
	patch := client.MergeFromWithOptions(latest.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[b2backupv1.AnnotationRestoreLock] = lock.id
	if !latest.Spec.Suspend {
		latest.Spec.Suspend = true
		latest.Annotations[b2backupv1.AnnotationRestoreResume] = restoreResumeTrue
	}
	lock.suspended = latest.Annotations[b2backupv1.AnnotationRestoreResume] == restoreResumeTrue
	if err := c.Patch(ctx, &latest, patch); err != nil {
		if apierrors.IsConflict(err) {
			return nil, fmt.Errorf("%w: scheduledbackup/%s changed while it was being locked; try again",
				ErrRestoreRunning, sb.Name)
		}
		return nil, fmt.Errorf("suspending scheduledbackup/%s: %w", sb.Name, err)
	}
	sb.Spec.Suspend = true
	return lock, nil
}

// release lets the ScheduledBackup go. With good set, the restored data is
// complete or untouched, and a schedule the lock suspended is resumed;
// otherwise it stays suspended, and the next restore that succeeds resumes it.
func (l *restoreLock) release(good bool) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
	defer cancel()

	resumed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		resumed = false
		var latest b2backupv1.ScheduledBackup
		if err := l.c.Get(ctx, l.key, &latest); err != nil {
			return err
		}
		if holder := latest.Annotations[b2backupv1.AnnotationRestoreLock]; holder != l.id {
			return fmt.Errorf("%w: scheduledbackup/%s is now held by %q", ErrRestoreRunning, l.key.Name, holder)
		}
		patch := client.MergeFromWithOptions(latest.DeepCopy(), client.MergeFromWithOptimisticLock{})
		delete(latest.Annotations, b2backupv1.AnnotationRestoreLock)
		if good && latest.Annotations[b2backupv1.AnnotationRestoreResume] == restoreResumeTrue {
			delete(latest.Annotations, b2backupv1.AnnotationRestoreResume)
			latest.Spec.Suspend = false
			resumed = true
		}
		return l.c.Patch(ctx, &latest, patch)
	})
	return resumed, err
}

// finish releases the lock after a restore that returned err, having written
// to the target or not, and says what became of the schedule.
func (l *restoreLock) finish(f *Factory, sb *b2backupv1.ScheduledBackup, err error, wrote bool, what string) error {
	if l == nil {
		return err
	}
	good := err == nil || !wrote
	_, releaseErr := l.release(good)
	p := newPrinter(f.Streams.ErrOut)
	if !good && l.suspended {
		p.printf("! scheduledbackup/%s stays suspended: %s may hold a partial restore, which the\n", sb.Name, what)
		p.printf("! next backup would save and retention could then keep over good snapshots.\n")
		p.printf("! Check it, then: b2bk resume %s\n", sb.Name)
	}
	if releaseErr != nil {
		p.printf("! could not release scheduledbackup/%s after the restore: %v\n", sb.Name, releaseErr)
		p.printf("! check spec.suspend and the %s annotation with: b2bk status %s\n",
			b2backupv1.AnnotationRestoreLock, sb.Name)
	}
	return errors.Join(err, p.Err())
}

// readyToWrite is checked in the restore pod, just before the first write: the
// operator has suspended the CronJob, and no backup or other restore started
// in the meantime.
func readyToWrite(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, ownJob string) error {
	var cj batchv1.CronJob
	key := types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}
	if err := c.Get(ctx, key, &cj); err != nil {
		return err
	}
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		return fmt.Errorf("%w: the operator has not suspended cronjob/%s yet, so a backup could start "+
			"during the restore; nothing was written, try again", ErrNotCurrent, cj.Name)
	}
	return refuseActive(ctx, c, sb, ownJob)
}

func refuseActive(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, ownJob string) error {
	job, err := activeBackup(ctx, c, sb, ownJob)
	if err != nil || job == "" {
		return err
	}
	return fmt.Errorf("%w: job/%s is running for scheduledbackup/%s; wait for it with b2bk wait %s, "+
		"or stop it with b2bk cancel %s", ErrBackupRunning, job, sb.Name, sb.Name, sb.Name)
}

// writingPurposes are the b2bk Jobs that write to a backup's claims or database.
var writingPurposes = []string{purposeRestore, purposeRestoreDB}

// activeBackup returns a Job of this ScheduledBackup that has not finished,
// other than ownJob: a backup the CronJob started, a triggered one, or a b2bk
// Job restoring into its claims or database. A b2bk Job from before Jobs
// recorded their purpose counts as a restore.
func activeBackup(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, ownJob string) (string, error) {
	owners := []types.UID{sb.UID}
	var cj batchv1.CronJob
	switch err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}, &cj); {
	case err == nil:
		owners = append(owners, cj.UID)
	case !apierrors.IsNotFound(err):
		return "", err
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(sb.Namespace)); err != nil {
		return "", err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == ownJob || backup.JobFinished(job) {
			continue
		}
		if owner := metav1.GetControllerOf(job); owner != nil && slices.Contains(owners, owner.UID) {
			return job.Name, nil
		}
		if job.Labels[labelManagedBy] != runner.ManagedByValue || !ownedBy(job, sb.UID) {
			continue
		}
		if purpose, ok := job.Labels[runner.LabelPurpose]; !ok || slices.Contains(writingPurposes, purpose) {
			return job.Name, nil
		}
	}
	return "", nil
}

func ownedBy(obj metav1.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}
