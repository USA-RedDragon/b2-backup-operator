package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

// ErrUnhealthy means at least one doctor check failed.
var ErrUnhealthy = errors.New("one or more checks failed")

type checkLevel int

const (
	levelOK checkLevel = iota
	levelInfo
	levelWarn
	levelFail
)

func (l checkLevel) symbol() string {
	switch l {
	case levelOK:
		return "✔"
	case levelInfo:
		return "·"
	case levelWarn:
		return "!"
	default:
		return "✖"
	}
}

type check struct {
	level   checkLevel
	summary string
	detail  []string
}

type report struct {
	subject string
	checks  []check
}

func (r *report) add(level checkLevel, summary string, detail ...string) {
	r.checks = append(r.checks, check{level: level, summary: summary, detail: detail})
}

func (r *report) failed() bool {
	for _, c := range r.checks {
		if c.level == levelFail {
			return true
		}
	}
	return false
}

func (r *report) write(p *printer) {
	p.println(r.subject)
	for _, c := range r.checks {
		p.printf("  %s %s\n", c.level.symbol(), c.summary)
		for _, d := range c.detail {
			p.printf("      %s\n", d)
		}
	}
}

func newDoctorCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "doctor [name]",
		Short:   "Explain why a backup is not running",
		GroupID: GroupInspect,
		Long: `Diagnose repositories and scheduled backups.

Checks the things that actually break: a missing or unreadable credentials
Secret, a CronJob owned by something else, a repository that never initialized,
and a most-recent run that did not succeed.

With no argument every resource in the namespace is checked. Exits non-zero if
any check fails.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}

			var arg string
			if len(args) == 1 {
				arg = args[0]
			}
			return runDoctor(cmd.Context(), c, f.Streams.Out, ns, arg)
		},
	}
	return cmd
}

func runDoctor(ctx context.Context, c client.Client, out io.Writer, namespace, arg string) error {
	reports, err := collectReports(ctx, c, namespace, arg)
	if err != nil {
		return err
	}

	p := newPrinter(out)

	if len(reports) == 0 {
		p.printf("No resources found in namespace %q.\n", namespace)
		return p.Err()
	}

	unhealthy := false
	for i, r := range reports {
		if i > 0 {
			p.println()
		}
		r.write(p)
		unhealthy = unhealthy || r.failed()
	}

	if err := p.Err(); err != nil {
		return err
	}
	if unhealthy {
		return ErrUnhealthy
	}
	return nil
}

func collectReports(ctx context.Context, c client.Client, namespace, arg string) ([]*report, error) {
	if arg != "" {
		target, err := Resolve(ctx, c, namespace, arg)
		if err != nil {
			return nil, err
		}
		if target.Kind == TargetRepository {
			return []*report{checkRepository(ctx, c, target.Repository)}, nil
		}
		return []*report{checkScheduledBackup(ctx, c, target.ScheduledBackup)}, nil
	}

	var repos b2backupv1.RepositoryList
	if err := c.List(ctx, &repos, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var backups b2backupv1.ScheduledBackupList
	if err := c.List(ctx, &backups, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	reports := make([]*report, 0, len(repos.Items)+len(backups.Items))
	for i := range repos.Items {
		reports = append(reports, checkRepository(ctx, c, &repos.Items[i]))
	}
	for i := range backups.Items {
		reports = append(reports, checkScheduledBackup(ctx, c, &backups.Items[i]))
	}
	return reports, nil
}

func checkRepository(ctx context.Context, c client.Client, repo *b2backupv1.Repository) *report {
	r := &report{subject: "repository/" + repo.Name}

	if repo.Spec.Suspend {
		r.add(levelWarn, "reconciliation is suspended",
			"The operator will not touch this repository until spec.suspend is cleared.")
		return r
	}

	checkGeneration(r, repo.Generation, repo.Status.ObservedGeneration)
	checkCondition(r, repo.Status.Conditions, b2backupv1.RepositoryConditionReady, "repository")

	if cond := FindCondition(repo.Status.Conditions, b2backupv1.RepositoryConditionReady); cond != nil &&
		cond.Reason == "RepositoryConflict" {
		return r
	}

	switch {
	case repo.Status.Initialized:
		r.add(levelOK, "restic repository is initialized")
	default:
		r.add(levelFail, "restic repository is not initialized")
		describeInitJobs(ctx, c, repo, r)
	}

	switch {
	case repo.Status.BucketID == "":
		r.add(levelWarn, "no B2 bucket recorded yet")
	case repo.Status.Adopted:
		r.add(levelInfo, fmt.Sprintf("adopted the existing restic repository at %s/%s",
			repo.Status.BucketName, repositoryPath(repo)))
	default:
		r.add(levelInfo, fmt.Sprintf("stored at %s/%s", repo.Status.BucketName, repositoryPath(repo)))
	}

	if cond := FindCondition(repo.Status.Conditions, b2backupv1.RepositoryConditionLifecycleRule); cond != nil &&
		cond.Status != metav1.ConditionTrue {
		r.add(levelWarn, fmt.Sprintf("lifecycle rule: %s", cond.Reason), cond.Message)
	}

	checkCredentialsSecret(ctx, c, repo, r)
	return r
}

func checkScheduledBackup(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup) *report {
	r := &report{subject: "scheduledbackup/" + sb.Name}

	if sb.Spec.Suspend {
		r.add(levelWarn, "backups are suspended",
			"The CronJob is kept but will not fire. Clear spec.suspend to resume.")
	}

	checkGeneration(r, sb.Generation, sb.Status.ObservedGeneration)
	checkCondition(r, sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady, "backup")

	repo, err := RepositoryFor(ctx, c, sb)
	switch {
	case apierrors.IsForbidden(err):
		skipForbidden(r, fmt.Sprintf("repository %q", sb.Spec.RepositoryRef.Name))
	case errors.Is(err, ErrTargetNotFound):
		r.add(levelFail, fmt.Sprintf("repository %q does not exist", sb.Spec.RepositoryRef.Name),
			"spec.repositoryRef must name a Repository in this namespace.")
	case err != nil:
		r.add(levelFail, "could not read the referenced repository", err.Error())
	default:
		checkReferencedRepository(ctx, c, repo, r)
	}

	checkCronJob(ctx, c, sb, r)
	checkCachePVC(ctx, c, sb, r)
	checkLastRun(sb, r)
	return r
}

func checkReferencedRepository(ctx context.Context, c client.Client, repo *b2backupv1.Repository, r *report) {
	ready := FindCondition(repo.Status.Conditions, b2backupv1.RepositoryConditionReady)
	switch {
	case !repo.Status.Initialized:
		r.add(levelFail, fmt.Sprintf("repository %q is not initialized", repo.Name),
			"Backups cannot run until `restic init` succeeds. Run: b2bk doctor repo/"+repo.Name)
	case ready == nil || ready.Status != metav1.ConditionTrue:
		r.add(levelWarn, fmt.Sprintf("repository %q is not ready", repo.Name),
			"Run: b2bk doctor repo/"+repo.Name)
	default:
		r.add(levelOK, fmt.Sprintf("repository %q is ready and initialized", repo.Name))
	}

	checkCredentialsSecret(ctx, c, repo, r)
}

func checkCredentialsSecret(ctx context.Context, c client.Client, repo *b2backupv1.Repository, r *report) {
	name := repo.SecretName()

	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: repo.Namespace, Name: name}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		r.add(levelFail, fmt.Sprintf("credentials Secret %q does not exist", name),
			"The operator writes it on reconcile; if it is missing the repository has not reconciled.")
		return
	case apierrors.IsForbidden(err):
		skipForbidden(r, fmt.Sprintf("Secret %q", name))
		return
	case err != nil:
		r.add(levelFail, fmt.Sprintf("could not read Secret %q", name), err.Error())
		return
	}

	var missing []string
	for _, key := range controller.SecretKeys {
		if len(secret.Data[key]) == 0 {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		r.add(levelFail, fmt.Sprintf("Secret %q is missing %v", name, missing),
			"The operator rewrites it on its next reconcile; restic cannot reach B2 until then.")
		return
	}

	keyID := string(secret.Data[controller.KeyAccessKeyID])
	if recorded := repo.Status.ApplicationKeyID; recorded != "" && keyID != recorded {
		r.add(levelFail, fmt.Sprintf("Secret %q holds application key %s, but the operator recorded %s",
			name, keyID, recorded),
			"The key in the Secret may have been deleted. The operator replaces it on its next reconcile.")
		return
	}
	r.add(levelOK, fmt.Sprintf("credentials Secret %q has all %d keys, application key %s",
		name, len(controller.SecretKeys), keyID))
}

func checkCronJob(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, r *report) {
	name := backup.CronJobName(sb)

	var cj batchv1.CronJob
	err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, &cj)
	switch {
	case apierrors.IsNotFound(err):
		r.add(levelFail, fmt.Sprintf("CronJob %q does not exist", name),
			"The operator creates it once the backup is ready.")
		return
	case apierrors.IsForbidden(err):
		skipForbidden(r, fmt.Sprintf("CronJob %q", name))
		return
	case err != nil:
		r.add(levelFail, fmt.Sprintf("could not read CronJob %q", name), err.Error())
		return
	}

	if !metav1.IsControlledBy(&cj, sb) {
		detail := []string{
			"The operator adopts a CronJob carrying its own managed-by label and rewrites its owner.",
			"Without that label the operator cannot see it, and creating its own fails instead.",
		}
		if owner := metav1.GetControllerOf(&cj); owner != nil {
			detail = append(detail, fmt.Sprintf("It is controlled by %s/%s.", owner.Kind, owner.Name))
		}
		detail = append(detail,
			"Remove whatever created it, then delete cronjob/"+name+
				", unless the operator should take it over.")
		r.add(levelFail, fmt.Sprintf("CronJob %q is not controlled by this ScheduledBackup", name), detail...)
		return
	}

	r.add(levelOK, fmt.Sprintf("CronJob %q is owned by this ScheduledBackup", name))

	if suspended := cj.Spec.Suspend != nil && *cj.Spec.Suspend; suspended != sb.Spec.Suspend {
		r.add(levelWarn, "CronJob suspend state does not match spec.suspend",
			fmt.Sprintf("CronJob suspended=%t, spec.suspend=%t. The operator has not reconciled yet.",
				suspended, sb.Spec.Suspend))
	}
}

func checkCachePVC(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, r *report) {
	if sb.Spec.Cache != nil && sb.Spec.Cache.Enabled != nil && !*sb.Spec.Cache.Enabled {
		return
	}
	name := backup.CacheName(sb)

	var pvc corev1.PersistentVolumeClaim
	err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		r.add(levelWarn, fmt.Sprintf("cache PVC %q does not exist", name),
			"Backups still run, but restic re-reads the repository index every time.")
	case apierrors.IsForbidden(err):
		skipForbidden(r, fmt.Sprintf("cache PVC %q", name))
	case err != nil:
		r.add(levelWarn, fmt.Sprintf("could not read PVC %q", name), err.Error())
	case pvc.Status.Phase != corev1.ClaimBound:
		r.add(levelWarn, fmt.Sprintf("cache PVC %q is %s, not Bound", name, pvc.Status.Phase),
			"A pending claim will block the backup pod from starting.")
	default:
		r.add(levelOK, fmt.Sprintf("cache PVC %q is bound", name))
	}
}

func checkLastRun(sb *b2backupv1.ScheduledBackup, r *report) {
	st := sb.Status

	if st.Active > 0 {
		r.add(levelInfo, fmt.Sprintf("%d backup(s) running now", st.Active))
		return
	}

	if len(st.History) > 0 {
		checkRecordedRuns(st.History, r, sb.Name)
		return
	}

	switch {
	case st.LastScheduleTime == nil:

		r.add(levelWarn, "no backup has run yet",
			"The first run happens at the next scheduled slot.",
			"To take one now: b2bk backup "+sb.Name)
	case st.LastSuccessfulTime == nil:
		r.add(levelFail, fmt.Sprintf("a backup started %s but none has ever succeeded", Since(st.LastScheduleTime)),
			"Run: b2bk logs "+sb.Name)
	case st.LastSuccessfulTime.Before(st.LastScheduleTime):
		r.add(levelFail, fmt.Sprintf("the most recent run (%s) did not succeed", Since(st.LastScheduleTime)),
			fmt.Sprintf("Last success was %s.", Since(st.LastSuccessfulTime)),
			"Run: b2bk logs "+sb.Name)
	default:
		r.add(levelOK, fmt.Sprintf("last backup succeeded %s", Since(st.LastSuccessfulTime)))
	}
}

func skipForbidden(r *report, subject string) {
	r.add(levelInfo, "skipped "+subject+": not allowed to read it",
		"This is an RBAC limit on your account, not a problem with the resource.")
}

func checkGeneration(r *report, generation, observed int64) {
	if observed != 0 && observed < generation {
		r.add(levelWarn, "the operator has not caught up with the latest spec change",
			fmt.Sprintf("generation %d, observed %d.", generation, observed))
	}
}

func checkCondition(r *report, conds []metav1.Condition, condType, noun string) {
	cond := FindCondition(conds, condType)
	switch {
	case cond == nil:
		r.add(levelWarn, "the operator has not reconciled this "+noun+" yet")
	case cond.Status == metav1.ConditionTrue:
		r.add(levelOK, noun+" is ready")
	default:
		detail := []string{}
		if cond.Message != "" {
			detail = append(detail, cond.Message)
		}
		r.add(levelFail, fmt.Sprintf("%s is not ready (%s)", noun, cond.Reason), detail...)
	}
}

func describeInitJobs(ctx context.Context, c client.Client, repo *b2backupv1.Repository, r *report) {
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(repo.Namespace)); err != nil {
		if apierrors.IsForbidden(err) {
			skipForbidden(r, "the init Job")
			return
		}
		r.add(levelInfo, "could not list Jobs to find the init Job", err.Error())
		return
	}

	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !metav1.IsControlledBy(job, repo) {
			continue
		}
		switch {
		case job.Status.Failed > 0:
			detail := []string{"Run: kubectl logs job/" + job.Name}
			for _, cond := range job.Status.Conditions {
				if cond.Type == batchv1.JobFailed && cond.Message != "" {
					detail = append([]string{cond.Message}, detail...)
				}
			}
			r.add(levelInfo, fmt.Sprintf("init Job %q has failed %d time(s)", job.Name, job.Status.Failed), detail...)
		case job.Status.Active > 0:
			r.add(levelInfo, fmt.Sprintf("init Job %q is running", job.Name))
		default:
			r.add(levelInfo, fmt.Sprintf("init Job %q is neither running nor failed", job.Name),
				"The operator has not recorded its result on the Repository yet.")
		}
		return
	}

	r.add(levelInfo, "no init Job exists yet")
}

func checkRecordedRuns(history []b2backupv1.BackupRun, r *report, name string) {
	last := history[0]
	when := Since(last.CompletionTime)
	if last.CompletionTime == nil {
		when = Since(last.StartTime)
	}

	switch last.Result {
	case b2backupv1.BackupRunSucceeded:
		r.add(levelOK, fmt.Sprintf("last backup succeeded %s (%s)", when, strings.ToLower(string(last.Trigger))))
	case b2backupv1.BackupRunRunning:
		r.add(levelInfo, fmt.Sprintf("a backup started %s is still running", Since(last.StartTime)))
	default:
		r.add(levelFail, fmt.Sprintf("the most recent run (%s) failed", when),
			"Run: b2bk logs "+name)
	}
}
