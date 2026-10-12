package tui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

var errNoBackups = errors.New("RBAC does not let you list and watch scheduledbackups here; ? says why")

const (
	labelManagedBy  = "app.kubernetes.io/managed-by"
	operatorManaged = "b2-backup-operator"
)

// clusterState is what the watches hold, grouped by ScheduledBackup.
type clusterState struct {
	rows  []*row
	repos map[types.NamespacedName]*b2backupv1.Repository
}

// row is one ScheduledBackup and everything the dashboard shows about it.
type row struct {
	sb   *b2backupv1.ScheduledBackup
	repo *b2backupv1.Repository
	cj   *batchv1.CronJob
	// jobs are the backup's own Jobs, scheduled and triggered, newest first.
	jobs []batchv1.Job
	// pods are the pods of jobs, by Job name.
	pods map[string][]corev1.Pod
	// b2bkPods are the pods of b2bk's one-off Jobs for this backup.
	b2bkPods []corev1.Pod
	hold     string
}

func (r *row) key() types.NamespacedName {
	return types.NamespacedName{Namespace: r.sb.Namespace, Name: r.sb.Name}
}

func (r *row) title() string { return r.sb.Namespace + "/" + r.sb.Name }

// buildState lists what the watches hold in namespace, or in every namespace.
func buildState(ctx context.Context, rd client.Reader, namespace string, kinds []Kind) (*clusterState, error) {
	if !slices.Contains(kinds, KindScheduledBackup) {
		return nil, errNoBackups
	}
	in := client.InNamespace(namespace)
	var backups b2backupv1.ScheduledBackupList
	if err := rd.List(ctx, &backups, in); err != nil {
		return nil, err
	}
	var repos b2backupv1.RepositoryList
	if slices.Contains(kinds, KindRepository) {
		if err := rd.List(ctx, &repos, in); err != nil {
			return nil, err
		}
	}
	var cronJobs batchv1.CronJobList
	if slices.Contains(kinds, KindCronJob) {
		if err := rd.List(ctx, &cronJobs, in); err != nil {
			return nil, err
		}
	}
	var jobs batchv1.JobList
	if slices.Contains(kinds, KindJob) {
		if err := rd.List(ctx, &jobs, in); err != nil {
			return nil, err
		}
	}
	var pods corev1.PodList
	if slices.Contains(kinds, KindPod) {
		if err := rd.List(ctx, &pods, in); err != nil {
			return nil, err
		}
	}
	return groupState(backups.Items, repos.Items, cronJobs.Items, jobs.Items, pods.Items), nil
}

func groupState(
	backups []b2backupv1.ScheduledBackup, repos []b2backupv1.Repository, cronJobs []batchv1.CronJob,
	jobs []batchv1.Job, pods []corev1.Pod,
) *clusterState {
	st := &clusterState{repos: map[types.NamespacedName]*b2backupv1.Repository{}}
	for i := range repos {
		st.repos[types.NamespacedName{Namespace: repos[i].Namespace, Name: repos[i].Name}] = &repos[i]
	}
	cjs := map[types.NamespacedName]*batchv1.CronJob{}
	for i := range cronJobs {
		cjs[types.NamespacedName{Namespace: cronJobs[i].Namespace, Name: cronJobs[i].Name}] = &cronJobs[i]
	}
	byNamespace := map[string][]b2backupv1.ScheduledBackup{}
	for i := range backups {
		byNamespace[backups[i].Namespace] = append(byNamespace[backups[i].Namespace], backups[i])
	}
	podsByJob := map[types.NamespacedName][]corev1.Pod{}
	for i := range pods {
		if job := podJob(&pods[i]); job != "" {
			k := types.NamespacedName{Namespace: pods[i].Namespace, Name: job}
			podsByJob[k] = append(podsByJob[k], pods[i])
		}
	}

	for i := range backups {
		sb := &backups[i]
		r := &row{sb: sb, pods: map[string][]corev1.Pod{}}
		r.repo = st.repos[types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name}]
		r.cj = cjs[types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}]
		r.hold = backup.RestoreHold(sb)
		if r.hold == "" {
			r.hold = backup.SharedRestoreHold(sb, byNamespace[sb.Namespace])
		}
		owners := map[types.UID]bool{sb.UID: true}
		if r.cj != nil {
			owners[r.cj.UID] = true
		}
		for j := range jobs {
			job := &jobs[j]
			if job.Namespace != sb.Namespace {
				continue
			}
			k := types.NamespacedName{Namespace: job.Namespace, Name: job.Name}
			if owner := metav1.GetControllerOf(job); owner != nil && owners[owner.UID] {
				r.jobs = append(r.jobs, *job)
				r.pods[job.Name] = podsByJob[k]
			}
		}
		slices.SortStableFunc(r.jobs, func(a, b batchv1.Job) int { return jobStart(&b).Compare(jobStart(&a)) })
		for j := range pods {
			pod := &pods[j]
			if pod.Namespace == sb.Namespace && pod.Labels[labelManagedBy] == runner.ManagedByValue &&
				pod.Labels[runner.LabelScheduledBackup] == sb.Name {
				r.b2bkPods = append(r.b2bkPods, *pod)
			}
		}
		st.rows = append(st.rows, r)
	}
	slices.SortFunc(st.rows, func(a, b *row) int {
		return cmp.Or(cmp.Compare(a.sb.Namespace, b.sb.Namespace), cmp.Compare(a.sb.Name, b.sb.Name))
	})
	return st
}

func podJob(pod *corev1.Pod) string {
	if name := pod.Labels[batchv1.JobNameLabel]; name != "" {
		return name
	}
	return pod.Labels["job-name"]
}

func jobStart(job *batchv1.Job) time.Time {
	if job.Status.StartTime != nil {
		return job.Status.StartTime.Time
	}
	return job.CreationTimestamp.Time
}

func jobFinished(job *batchv1.Job) bool {
	return backup.JobFinished(job) || (job.Status.Active == 0 && (job.Status.Succeeded > 0 || job.Status.Failed > 0))
}

// running is the backup's Job in flight, if any.
func (r *row) running() *batchv1.Job {
	for i := range r.jobs {
		if !jobFinished(&r.jobs[i]) {
			return &r.jobs[i]
		}
	}
	return nil
}

// health is how a backup is doing, worst first.
type health int

const (
	healthFailed health = iota
	healthNotReady
	healthHeld
	healthRunning
	healthSuspended
	healthWaiting
	healthOK
)

func (r *row) health() health {
	ready := apimeta.FindStatusCondition(r.sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady)
	switch {
	case r.running() != nil || r.sb.Status.Active > 0:
		return healthRunning
	case ready == nil || ready.Status != metav1.ConditionTrue:
		return healthNotReady
	case r.lastRunFailed():
		return healthFailed
	case r.hold != "":
		return healthHeld
	case r.sb.Spec.Suspend:
		return healthSuspended
	case backup.LastSuccess(r.sb) == nil:
		return healthWaiting
	}
	return healthOK
}

func (r *row) lastRunFailed() bool {
	if h := r.sb.Status.History; len(h) > 0 {
		return h[0].Result == b2backupv1.BackupRunFailed
	}
	for i := range r.jobs {
		if r.jobs[i].Status.Failed > 0 {
			return true
		}
		if r.jobs[i].Status.Succeeded > 0 {
			return false
		}
	}
	return false
}

// status is the dashboard's STATUS cell.
func (r *row) status(now time.Time) string {
	switch r.health() {
	case healthRunning:
		text := symRunning + " running"
		if job := r.running(); job != nil {
			text += "  " + clock(now.Sub(jobStart(job)))
		}
		return styleWarn.Render(text)
	case healthNotReady:
		reason := "not reconciled"
		if c := apimeta.FindStatusCondition(r.sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady); c != nil {
			reason = c.Reason
		}
		return styleFail.Render(symWarn + " " + reason)
	case healthFailed:
		text := symFail + " failed"
		if exit := r.lastExit(); exit != "" {
			text += "   " + exit
		}
		return styleFail.Render(text)
	case healthHeld:
		return styleWarn.Render(symPaused + " held")
	case healthSuspended:
		return styleDim.Render(symPaused + " suspended")
	case healthWaiting:
		return styleDim.Render(symWaiting + " no runs yet")
	}
	return styleOK.Render(symOK + " ok")
}

// lastExit describes how the newest failed Job's container ended.
func (r *row) lastExit() string {
	for i := range r.jobs {
		if r.jobs[i].Status.Failed > 0 {
			return jobExit(&r.jobs[i], r.pods[r.jobs[i].Name])
		}
	}
	return ""
}

func jobExit(job *batchv1.Job, pods []corev1.Pod) string {
	for i := range pods {
		for _, cs := range pods[i].Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				return fmt.Sprintf("exit %d", t.ExitCode)
			}
		}
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason != "" {
			return c.Reason
		}
	}
	return ""
}

// location is the backup's time zone, the CronJob's when spec.timeZone is unset.
func (r *row) location() *time.Location {
	tz := r.sb.Spec.TimeZone
	if tz == "" && r.cj != nil && r.cj.Spec.TimeZone != nil {
		tz = *r.cj.Spec.TimeZone
	}
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

func (r *row) schedule() string {
	if s := r.sb.Status.EffectiveSchedule; s != "" {
		return s
	}
	if r.cj != nil {
		return r.cj.Spec.Schedule
	}
	return ""
}

// nextRun is when the CronJob fires next, or false when it will not fire.
func (r *row) nextRun(now time.Time) (time.Time, bool) {
	if r.sb.Spec.Suspend || r.hold != "" {
		return time.Time{}, false
	}
	if r.cj != nil && r.cj.Spec.Suspend != nil && *r.cj.Spec.Suspend {
		return time.Time{}, false
	}
	expr := r.schedule()
	if expr == "" {
		return time.Time{}, false
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, false
	}
	next := sched.Next(now.In(r.location()))
	return next, !next.IsZero()
}

// sources is the dashboard's SOURCES cell: files /data, db, script.
// usage is the size of the backup's repository as the operator last measured
// it, or an em dash before it has.
func (r *row) usage() string {
	if r.repo == nil || r.repo.Status.CurrentUsage == "" {
		return symEm
	}
	return usageText(r.repo.Status.CurrentUsage)
}

var usageForm = regexp.MustCompile(`^([0-9.]+)\s*([KMGTPE]iB|B)$`)

// usageText spaces a size the operator recorded as 246KiB the way restic
// prints one, 246 KiB, so the two read alike side by side.
func usageText(s string) string {
	if m := usageForm.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1] + " " + m[2]
	}
	return s
}

func (r *row) sources() string {
	if r.sb.Spec.Script != "" && len(r.sb.Spec.Sources) == 0 {
		return "script"
	}
	var parts []string
	for i := range r.sb.Spec.Sources {
		src := &r.sb.Spec.Sources[i]
		if src.Type != b2backupv1.SourceTypeFiles {
			parts = append(parts, "db")
			continue
		}
		paths := src.ResolvedPaths(r.sb.EffectiveWorkingDir())
		text := "files " + paths[0]
		if len(paths) > 1 {
			text += fmt.Sprintf(" +%d", len(paths)-1)
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, ", ")
}

// region is the B2 region every Repository's endpoint names, with a count of
// the others when they differ.
func (st *clusterState) region() string {
	var regions []string
	for _, repo := range st.repos {
		u, err := url.Parse(repo.Status.Endpoint)
		if err != nil || u.Host == "" {
			continue
		}
		host := strings.TrimPrefix(u.Host, "s3.")
		region, _, _ := strings.Cut(host, ".")
		if region != "" && !slices.Contains(regions, region) {
			regions = append(regions, region)
		}
	}
	slices.Sort(regions)
	switch len(regions) {
	case 0:
		return ""
	case 1:
		return regions[0]
	}
	return fmt.Sprintf("%s +%d", regions[0], len(regions)-1)
}
