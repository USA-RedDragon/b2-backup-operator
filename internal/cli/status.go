package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const defaultRunLimit = 10

func newStatusCommand(f *Factory) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:     "status [NAME]",
		Short:   "Show a backup or repository in detail",
		GroupID: GroupInspect,
		Long: `Show one resource in detail, with its recent runs.

Runs are read from the Jobs still present in the cluster. Jobs are removed an
hour after they finish and pruned by the CronJob history limits, so this shows
recent history rather than a complete record.

With no NAME, it shows the namespace's only ScheduledBackup, or its only
Repository when it has no ScheduledBackup.`,
		Args: argCount(0, 1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			arg, err := f.nameArg(cmd, args, anyKind)
			if err != nil {
				return err
			}
			return runStatus(cmd.Context(), c, f.Streams.Out, ns, arg, limit)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", defaultRunLimit, "Maximum number of runs to show")
	return cmd
}

func runStatus(ctx context.Context, c client.Client, out io.Writer, namespace, arg string, limit int) error {
	target, err := Resolve(ctx, c, namespace, arg)
	if err != nil {
		return err
	}

	p := newPrinter(out)

	if target.Kind == TargetRepository {
		writeRepositoryStatus(p, target.Repository)
		return p.Err()
	}

	sb := target.ScheduledBackup
	repo, repoErr := RepositoryFor(ctx, c, sb)
	hold, err := currentRestoreHold(ctx, c, sb)
	if err != nil {
		hold = fmt.Sprintf("unknown: %v", err)
	}
	writeBackupStatus(p, sb, repo, repoErr, hold)

	jobs, err := BackupJobs(ctx, c, sb)
	if err != nil {
		return err
	}
	writeRuns(p, jobs, limit)
	return p.Err()
}

func writeRepositoryStatus(p *printer, repo *b2backupv1.Repository) {
	p.printf("repository/%s\n\n", repo.Name)

	tw := NewTabWriter(p)
	w := newPrinter(tw)
	field(w, "Mode", orNone(string(repo.Spec.Mode)))
	field(w, "Bucket", orNone(repo.Status.BucketName))
	field(w, "Path", repositoryPath(repo))
	field(w, "Endpoint", orNone(repo.Status.Endpoint))
	field(w, "Key ID", orNone(repo.Status.ApplicationKeyID))
	field(w, "Usage", usageOf(repo))
	field(w, "Initialized", yesNo(repo.Status.Initialized))
	field(w, "Adopted", yesNo(repo.Status.Adopted))
	field(w, "Suspended", yesNo(repo.Spec.Suspend))
	field(w, "Secret", orNone(repo.SecretName()))
	field(w, "Age", Age(repo.CreationTimestamp))
	_ = flushTable(w, tw)

	writeConditions(p, repo.Status.Conditions)
}

func writeBackupStatus(
	p *printer, sb *b2backupv1.ScheduledBackup, repo *b2backupv1.Repository, repoErr error, hold string,
) {
	p.printf("scheduledbackup/%s\n\n", sb.Name)

	tw := NewTabWriter(p)
	w := newPrinter(tw)
	if repoErr != nil {
		field(w, "Repository", fmt.Sprintf("%s (unreadable: %v)", sb.Spec.RepositoryRef.Name, repoErr))
	} else {
		field(w, "Repository", fmt.Sprintf("%s   %s", repo.Name, usageOf(repo)))
	}
	field(w, "Schedule", fmt.Sprintf("%s   %s",
		orNone(sb.Status.EffectiveSchedule), orNone(sb.Spec.TimeZone)))
	field(w, "Suspended", yesNo(sb.Spec.Suspend))
	if hold != "" {
		field(w, "Held", hold)
	}
	field(w, "Active", fmt.Sprintf("%d", sb.Status.Active))
	field(w, "Last scheduled", Since(sb.Status.LastScheduleTime))
	field(w, "Last successful", Since(lastSuccess(sb)))
	field(w, "Age", Age(sb.CreationTimestamp))
	_ = flushTable(w, tw)

	writeConditions(p, sb.Status.Conditions)
}

func usageOf(repo *b2backupv1.Repository) string {
	if repo.Status.CurrentUsage == "" {
		return "<unknown>"
	}
	if repo.Status.UsageTime == nil {
		return repo.Status.CurrentUsage
	}
	return repo.Status.CurrentUsage + " (measured " + Since(repo.Status.UsageTime) + ")"
}

func writeConditions(p *printer, conds []metav1.Condition) {
	if len(conds) == 0 {
		return
	}
	p.println()
	tw := NewTabWriter(p)
	w := newPrinter(tw)
	w.println("CONDITION\tSTATUS\tREASON\tMESSAGE")
	for i := range conds {
		w.printf("%s\t%s\t%s\t%s\n",
			conds[i].Type, conds[i].Status, conds[i].Reason, orNone(conds[i].Message))
	}
	_ = flushTable(w, tw)
}

func writeRuns(p *printer, jobs []batchv1.Job, limit int) {
	p.println()
	if len(jobs) == 0 {
		p.println("No runs are still present in the cluster.")
		return
	}
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}

	tw := NewTabWriter(p)
	w := newPrinter(tw)
	w.println("RESULT\tSTARTED\tDURATION\tJOB")
	for i := range jobs {
		job := &jobs[i]
		w.printf("%s\t%s\t%s\t%s\n",
			runResult(job), Since(job.Status.StartTime), runDuration(job), job.Name)
	}
	_ = flushTable(w, tw)
}

func runResult(job *batchv1.Job) string {
	switch {
	case job.Status.Succeeded > 0:
		return "succeeded"
	case job.Status.Failed > 0:
		return "failed"
	case job.Status.Active > 0:
		return "running"
	default:
		return "pending"
	}
}

func runDuration(job *batchv1.Job) string {
	if job.Status.StartTime == nil {
		return "-"
	}
	end := time.Now()
	if job.Status.CompletionTime != nil {
		end = job.Status.CompletionTime.Time
	} else if job.Status.Active == 0 && job.Status.Failed > 0 {
		for i := range job.Status.Conditions {
			cond := &job.Status.Conditions[i]
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				end = cond.LastTransitionTime.Time
				break
			}
		}
	}
	d := end.Sub(job.Status.StartTime.Time).Round(time.Second)
	if d < 0 {
		return "-"
	}
	return d.String()
}

func field(w *printer, name, value string) {
	w.printf("  %s\t%s\n", name, value)
}
