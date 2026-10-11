package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const historyLimit = 10

func (r *ScheduledBackupReconciler) reconcileTrigger(
	ctx context.Context,
	sb *b2backupv1.ScheduledBackup,
	repo *b2backupv1.Repository,
	runs []observedRun,
	hold string,
) error {
	raw := sb.Annotations[b2backupv1.AnnotationTriggerAt]
	if raw == "" {
		return nil
	}

	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, "TriggerInvalid", "Trigger",
			"Ignoring %s=%q: expected an RFC3339 timestamp", b2backupv1.AnnotationTriggerAt, raw)
		return nil
	}

	if sb.Status.LastTriggerTime != nil && !at.After(sb.Status.LastTriggerTime.Time) {
		return nil
	}

	if hold != "" {
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, "TriggerSkipped", "Trigger",
			"Not starting a backup: %s", hold)
		sb.Status.LastTriggerTime = &metav1.Time{Time: at}
		sb.Status.LastTriggerJob = ""
		return nil
	}

	wanted := backup.ManualJobName(sb, at)

	if sb.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent {
		if active := activeRun(runs); active != "" && active != wanted {
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, "TriggerSkipped", "Trigger",
				"Not starting a backup: Job %s is still running and concurrencyPolicy is Forbid", active)

			sb.Status.LastTriggerTime = &metav1.Time{Time: at}
			sb.Status.LastTriggerJob = ""
			return nil
		}
	}

	job, err := backup.BuildManualJob(sb, repo, r.Config, at)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(sb, job, r.Scheme); err != nil {
		return err
	}

	switch err := r.Create(ctx, job); {
	case err == nil:
		log.FromContext(ctx).Info("Started triggered backup Job", "job", job.Name)
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, "BackupTriggered", "Trigger",
			"Started backup Job %s", job.Name)
	case apierrors.IsAlreadyExists(err):
		// The Job name is derived from the trigger timestamp, so an already-existing
		// Job is this same trigger being reconciled again. Falling through would turn
		// a retry into an error.
	default:
		return fmt.Errorf("creating backup job: %w", err)
	}

	sb.Status.LastTriggerTime = &metav1.Time{Time: at}
	sb.Status.LastTriggerJob = job.Name
	return nil
}

type observedRun struct {
	run    b2backupv1.BackupRun
	active bool
}

func activeRun(runs []observedRun) string {
	for _, o := range runs {
		if o.active {
			return o.run.JobName
		}
	}
	return ""
}

func (r *ScheduledBackupReconciler) observedRuns(
	ctx context.Context, sb *b2backupv1.ScheduledBackup, cronJobUID types.UID,
) ([]observedRun, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(sb.Namespace)); err != nil {
		return nil, err
	}

	out := make([]observedRun, 0, len(jobs.Items))
	for i := range jobs.Items {
		job := &jobs.Items[i]
		owner := metav1.GetControllerOf(job)
		if owner == nil || (owner.UID != sb.UID && (cronJobUID == "" || owner.UID != cronJobUID)) {
			continue
		}
		out = append(out, runFromJob(job))
	}

	slices.SortStableFunc(out, func(a, b observedRun) int {
		return compareRunTimes(b.run.StartTime, a.run.StartTime)
	})
	return out, nil
}

func runFromJob(job *batchv1.Job) observedRun {
	trigger := b2backupv1.BackupTriggerScheduled
	if job.Labels[backup.LabelTrigger] == backup.TriggerManual {
		trigger = b2backupv1.BackupTriggerManual
	}

	run := b2backupv1.BackupRun{
		JobName:        job.Name,
		Trigger:        trigger,
		StartTime:      job.Status.StartTime,
		CompletionTime: job.Status.CompletionTime,
	}
	if run.StartTime == nil {
		run.StartTime = job.CreationTimestamp.DeepCopy()
	}

	switch {
	case job.Status.Succeeded > 0:
		run.Result = b2backupv1.BackupRunSucceeded
	case jobFailedAt(job) != nil:
		run.Result = b2backupv1.BackupRunFailed
	default:
		return observedRun{run: withResult(run, b2backupv1.BackupRunRunning), active: true}
	}
	return observedRun{run: run}
}

func withResult(run b2backupv1.BackupRun, result b2backupv1.BackupRunResult) b2backupv1.BackupRun {
	run.Result = result
	return run
}

func mergeHistory(existing []b2backupv1.BackupRun, observed []observedRun) []b2backupv1.BackupRun {
	merged := make([]b2backupv1.BackupRun, 0, len(existing)+len(observed))
	seen := make(map[string]int, len(existing)+len(observed))

	for _, o := range observed {
		seen[o.run.JobName] = len(merged)
		merged = append(merged, o.run)
	}
	for _, run := range existing {
		if i, ok := seen[run.JobName]; ok {
			merged[i] = mostComplete(run, merged[i])
			continue
		}
		seen[run.JobName] = len(merged)
		merged = append(merged, run)
	}

	slices.SortStableFunc(merged, func(a, b b2backupv1.BackupRun) int {
		return compareRunTimes(b.StartTime, a.StartTime)
	})
	if len(merged) > historyLimit {
		merged = merged[:historyLimit]
	}
	return merged
}

func mostComplete(recorded, live b2backupv1.BackupRun) b2backupv1.BackupRun {
	if live.StartTime == nil {
		live.StartTime = recorded.StartTime
	}
	if live.CompletionTime == nil {
		live.CompletionTime = recorded.CompletionTime
	}
	if live.Result == b2backupv1.BackupRunRunning && terminal(recorded.Result) {
		live.Result = recorded.Result
	}
	return live
}

func terminal(result b2backupv1.BackupRunResult) bool {
	return result == b2backupv1.BackupRunSucceeded || result == b2backupv1.BackupRunFailed
}

func compareRunTimes(a, b *metav1.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return a.Compare(b.Time)
}

// lastSucceeded is when the newest succeeded run in the history finished, or
// started when no completion was recorded. The CronJob's own
// lastSuccessfulTime counts only the Jobs it started, not triggered ones.
func lastSucceeded(history []b2backupv1.BackupRun) *metav1.Time {
	var latest *metav1.Time
	for i := range history {
		run := &history[i]
		if run.Result != b2backupv1.BackupRunSucceeded {
			continue
		}
		at := run.CompletionTime
		if at == nil {
			at = run.StartTime
		}
		latest = laterTime(latest, at)
	}
	return latest
}

// laterTime returns whichever of a and b is later, so a recorded time never
// moves back when the run that set it leaves the history.
func laterTime(a, b *metav1.Time) *metav1.Time {
	switch {
	case b == nil:
		return a
	case a == nil || b.After(a.Time):
		return b.DeepCopy()
	default:
		return a
	}
}
