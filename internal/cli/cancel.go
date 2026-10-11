package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func newCancelCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cancel <name>",
		Short:   "Stop the backup that is currently running",
		GroupID: GroupRun,
		Long: `Delete the Job of any backup currently in flight.

restic leaves a lock behind when it is killed mid-run. If the next backup then
reports a stale lock, clear it with: b2bk unlock REPOSITORY`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
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
				return fmt.Errorf("cancel needs a ScheduledBackup, not a %s", target.Kind)
			}
			return runCancel(cmd.Context(), c, f.Streams.Out, target.ScheduledBackup)
		},
	}
	return cmd
}

func runCancel(ctx context.Context, c client.Client, out io.Writer, sb *b2backupv1.ScheduledBackup) error {
	jobs, err := BackupJobs(ctx, c, sb)
	if err != nil {
		return err
	}

	p := newPrinter(out)
	cancelled := 0
	for i := range jobs {
		if !jobIsRunning(&jobs[i]) {
			continue
		}

		err := c.Delete(ctx, &jobs[i], client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting job/%s: %w", jobs[i].Name, err)
		}
		p.printf("cancelled job/%s\n", jobs[i].Name)
		cancelled++
	}

	if cancelled == 0 {
		p.printf("no backup is running for scheduledbackup/%s\n", sb.Name)
		return p.Err()
	}
	p.println("restic may have left a lock behind; if the next run reports one, use: b2bk unlock")
	return p.Err()
}

func jobIsRunning(job *batchv1.Job) bool {
	return job.Status.Succeeded == 0 && job.Status.Failed == 0
}

func newWaitCommand(f *Factory) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:     "wait <name>",
		Short:   "Block until the running backup finishes",
		GroupID: GroupRun,
		Long: `Wait for the backup currently in flight to finish.

Exits non-zero if it fails, so it can gate a deploy or a migration.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
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
				return fmt.Errorf("wait needs a ScheduledBackup, not a %s", target.Kind)
			}
			return runWait(cmd.Context(), c, f.Streams.Out, target.ScheduledBackup, timeout)
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", defaultWaitTimeout, "How long to wait")
	return cmd
}

func runWait(
	ctx context.Context,
	c client.Client,
	out io.Writer,
	sb *b2backupv1.ScheduledBackup,
	timeout time.Duration,
) error {
	jobs, err := BackupJobs(ctx, c, sb)
	if err != nil {
		return err
	}

	p := newPrinter(out)
	var running *batchv1.Job
	for i := range jobs {
		if jobIsRunning(&jobs[i]) {
			running = &jobs[i]
			break
		}
	}
	if running == nil {
		p.printf("no backup is running for scheduledbackup/%s\n", sb.Name)
		return p.Err()
	}

	p.printf("waiting for job/%s\n", running.Name)
	if err := p.Err(); err != nil {
		return err
	}

	final, err := waitForJob(ctx, c, client.ObjectKeyFromObject(running), timeout)
	if err != nil {
		return err
	}

	if final.Status.Failed > 0 {
		p.printf("job/%s failed\n", running.Name)
		if err := p.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: see `b2bk logs %s`", ErrBackupFailed, sb.Name)
	}
	p.printf("job/%s finished\n", running.Name)
	return p.Err()
}
