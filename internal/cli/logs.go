package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

// ErrNoRuns means the backup has never run.
var ErrNoRuns = errors.New("no backup runs found")

var jobNameLabels = []string{"batch.kubernetes.io/job-name", "job-name"}

func newLogsCommand(f *Factory) *cobra.Command {
	var (
		follow   bool
		previous bool
		tail     int64
	)

	cmd := &cobra.Command{
		Use:     "logs <name>",
		Short:   "Show logs from the most recent backup run",
		GroupID: GroupInspect,
		Long: `Stream the logs of the most recent backup Job.

Jobs are removed an hour after they finish, so this reaches only recent runs.`,
		Args: argCount(1, 1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			cs, err := f.Clientset()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}

			target, err := Resolve(cmd.Context(), c, ns, args[0])
			if err != nil {
				return err
			}
			if target.Kind != TargetScheduledBackup {
				return fmt.Errorf("logs needs a ScheduledBackup, not a %s", target.Kind)
			}

			opts := corev1.PodLogOptions{Follow: follow}
			if tail >= 0 {
				opts.TailLines = &tail
			}
			return runLogs(cmd.Context(), c, cs, f.Streams.Out, target.ScheduledBackup, previous, opts)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream new logs as they arrive")
	cmd.Flags().BoolVarP(&previous, "previous", "p", false, "Show the run before the most recent one")
	cmd.Flags().Int64Var(&tail, "tail", -1, "Show only this many trailing lines")
	return cmd
}

func runLogs(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	out io.Writer,
	sb *b2backupv1.ScheduledBackup,
	previous bool,
	opts corev1.PodLogOptions,
) error {
	jobs, err := BackupJobs(ctx, c, sb)
	if err != nil {
		return err
	}

	job, err := selectJob(jobs, previous)
	if err != nil {
		return fmt.Errorf("%w for scheduledbackup/%s", err, sb.Name)
	}

	p := newPrinter(out)
	p.printf("# job/%s (started %s)\n", job.Name, Since(job.Status.StartTime))
	if err := p.Err(); err != nil {
		return err
	}
	return streamJobLogs(ctx, c, cs, out, job, opts)
}

func selectJob(jobs []batchv1.Job, previous bool) (*batchv1.Job, error) {
	want := 0
	if previous {
		want = 1
	}
	if len(jobs) <= want {
		if previous && len(jobs) == 1 {
			return nil, fmt.Errorf("%w: only one run is still present", ErrNoRuns)
		}
		return nil, ErrNoRuns
	}
	return &jobs[want], nil
}

// BackupJobs returns the Jobs owned by a backup, whether scheduled or triggered.
func BackupJobs(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup) ([]batchv1.Job, error) {
	owners := map[types.UID]bool{sb.UID: true}

	var cj batchv1.CronJob
	err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}, &cj)
	switch {
	case err == nil:
		owners[cj.UID] = true
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	var all batchv1.JobList
	if err := c.List(ctx, &all, client.InNamespace(sb.Namespace)); err != nil {
		return nil, err
	}

	jobs := make([]batchv1.Job, 0, len(all.Items))
	for i := range all.Items {
		if owner := metav1.GetControllerOf(&all.Items[i]); owner != nil && owners[owner.UID] {
			jobs = append(jobs, all.Items[i])
		}
	}

	slices.SortStableFunc(jobs, func(a, b batchv1.Job) int {
		return jobStart(&b).Compare(jobStart(&a).Time)
	})
	return jobs, nil
}

func jobStart(job *batchv1.Job) metav1.Time {
	if job.Status.StartTime != nil {
		return *job.Status.StartTime
	}
	return job.CreationTimestamp
}

func streamJobLogs(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	out io.Writer,
	job *batchv1.Job,
	opts corev1.PodLogOptions,
) error {
	pods, err := podsForJob(ctx, c, job)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return fmt.Errorf("%w: job/%s has no pods left; they may have been garbage collected",
			ErrNoRuns, job.Name)
	}

	for i := range pods {
		if len(pods) > 1 {
			header := newPrinter(out)
			header.printf("# pod/%s\n", pods[i].Name)
			if err := header.Err(); err != nil {
				return err
			}
		}
		stream, err := cs.CoreV1().Pods(job.Namespace).GetLogs(pods[i].Name, &opts).Stream(ctx)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, stream)
		closeErr := stream.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func podsForJob(ctx context.Context, c client.Client, job *batchv1.Job) ([]corev1.Pod, error) {
	for _, label := range jobNameLabels {
		var pods corev1.PodList
		err := c.List(ctx, &pods,
			client.InNamespace(job.Namespace),
			client.MatchingLabels{label: job.Name},
		)
		if err != nil {
			return nil, err
		}
		if len(pods.Items) > 0 {
			return pods.Items, nil
		}
	}
	return nil, nil
}
