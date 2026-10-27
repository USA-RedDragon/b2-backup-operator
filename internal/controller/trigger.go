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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
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
	uid    types.UID
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
		o := runFromJob(job)
		o.uid = job.UID
		out = append(out, o)
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
		if run.CompletionTime == nil {
			run.CompletionTime = &metav1.Time{Time: *jobFailedAt(job)}
		}
	default:
		return observedRun{run: withResult(run, b2backupv1.BackupRunRunning), active: true}
	}
	return observedRun{run: run}
}

// activeRuns counts the runs in flight: the Jobs the CronJob lists as active
// and the ones observed running, triggered runs included, each once.
func activeRuns(cronActive []corev1.ObjectReference, runs []observedRun) int32 {
	names := map[string]bool{}
	for _, ref := range cronActive {
		names[ref.Name] = true
	}
	for _, o := range runs {
		if o.active {
			names[o.run.JobName] = true
		}
	}
	return int32(len(names))
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
	if len(live.Sources) == 0 {
		live.Sources = recorded.Sources
	}
	return live
}

// addRunSources reads what restic recorded for each finished run that has no
// sources in the history yet from the termination message of the run's pod.
// A script backup leaves none, so its runs are not looked up. Pods are read
// uncached: the manager caches no pods, and a cache of every pod in the
// cluster would cost far more than this list.
func (r *ScheduledBackupReconciler) addRunSources(
	ctx context.Context, sb *b2backupv1.ScheduledBackup, runs []observedRun,
) {
	if len(sb.Spec.Sources) == 0 {
		return
	}
	recorded := map[string]bool{}
	for _, run := range sb.Status.History {
		recorded[run.JobName] = len(run.Sources) > 0
	}
	wanted := map[types.UID]int{}
	for i := range runs {
		if !runs[i].active && runs[i].uid != "" && !recorded[runs[i].run.JobName] {
			wanted[runs[i].uid] = i
		}
	}
	if len(wanted) == 0 {
		return
	}
	uids := make([]string, 0, len(wanted))
	for uid := range wanted {
		uids = append(uids, string(uid))
	}
	slices.Sort(uids)
	req, err := labels.NewRequirement(batchv1.ControllerUidLabel, selection.In, uids)
	if err != nil {
		return
	}
	var pods corev1.PodList
	if err := r.uncached().List(ctx, &pods, client.InNamespace(sb.Namespace),
		client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*req)}); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list backup Job pods", "scheduledBackup", sb.Name)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			continue
		}
		at, ok := wanted[owner.UID]
		if !ok || len(runs[at].run.Sources) > 0 {
			continue
		}
		if sources := podRunSources(pod); len(sources) > 0 {
			runs[at].run.Sources = sources
		}
	}
}

// podRunSources reads the summaries the backup container of pod left.
func podRunSources(pod *corev1.Pod) []b2backupv1.BackupRunSource {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == backup.ContainerName && cs.State.Terminated != nil {
			return backup.ParseRunSummary(cs.State.Terminated.Message)
		}
	}
	return nil
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

// recordFailures updates lastFailureTime and consecutiveFailures from the run
// history and lastSuccessfulTime, which must already be current. The streak
// is read from the history, newest finished run first, back to the last
// success. When every run the history holds failed after that success, older
// failures may have left it, so the recorded count is carried forward and
// only failures newer than the recorded lastFailureTime are added to it. A
// pass that sees the same runs again counts nothing twice.
func recordFailures(status *b2backupv1.ScheduledBackupStatus) {
	recorded := status.LastFailureTime
	lastSuccess := status.LastSuccessfulTime

	finished := make([]b2backupv1.BackupRun, 0, len(status.History))
	for _, run := range status.History {
		if terminal(run.Result) {
			finished = append(finished, run)
		}
		if run.Result == b2backupv1.BackupRunFailed {
			status.LastFailureTime = laterTime(status.LastFailureTime, finishedAt(run))
		}
	}
	slices.SortStableFunc(finished, func(a, b b2backupv1.BackupRun) int {
		return compareRunTimes(finishedAt(b), finishedAt(a))
	})

	var streak, added int32
	bounded := len(status.History) < historyLimit
	for _, run := range finished {
		at := finishedAt(run)
		if run.Result == b2backupv1.BackupRunSucceeded || (lastSuccess != nil && !after(at, lastSuccess)) {
			bounded = true
			break
		}
		streak++
		if after(at, recorded) {
			added++
		}
	}

	if !bounded {
		var carried int32
		if after(recorded, lastSuccess) {
			carried = status.ConsecutiveFailures
		}
		streak = max(streak, carried+added)
	}
	status.ConsecutiveFailures = streak
}

// finishedAt is when run finished, or started when no completion was recorded.
func finishedAt(run b2backupv1.BackupRun) *metav1.Time {
	if run.CompletionTime != nil {
		return run.CompletionTime
	}
	return run.StartTime
}

// after reports whether a is set and later than b, or b is unset.
func after(a, b *metav1.Time) bool {
	return a != nil && (b == nil || a.After(b.Time))
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
