package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	pollInterval = time.Second

	defaultWaitTimeout = 2 * time.Hour
)

// Errors returned by the backup command.
var (
	ErrTriggerSkipped = errors.New("backup was not started")
	ErrBackupFailed   = errors.New("backup failed")

	errJobDeleted = errors.New("backup job was deleted before it finished")
)

func newBackupCommand(f *Factory) *cobra.Command {
	var (
		waitForIt bool
		follow    bool
		timeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:     "backup <name>",
		Aliases: []string{"run"},
		Short:   "Run a backup now",
		GroupID: GroupRun,
		Long: `Run a backup outside its schedule.

This annotates the ScheduledBackup and the operator does the work, so the run
is recorded in status and does exactly what a scheduled run would. The same
thing can be done with kubectl:

    kubectl annotate scheduledbackup/NAME \
      b2backup.mcswain.dev/trigger-at="$(date -Is)" --overwrite

A suspended ScheduledBackup still runs: suspending the schedule and then
backing up by hand is the main reason to suspend one. A run is refused only
when one is already in flight and concurrencyPolicy is Forbid.`,
		Args: cobra.ExactArgs(1),
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
				return fmt.Errorf("backup needs a ScheduledBackup, not a %s", target.Kind)
			}

			return runBackupNow(cmd.Context(), c, cs, f.Streams.Out,
				target.ScheduledBackup, waitForIt || follow, follow, timeout)
		},
	}

	cmd.Flags().BoolVar(&waitForIt, "wait", false, "Wait for the backup to finish")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream the backup's logs (implies --wait)")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultWaitTimeout, "How long to wait")
	return cmd
}

func runBackupNow(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	out io.Writer,
	sb *b2backupv1.ScheduledBackup,
	waitForIt, follow bool,
	timeout time.Duration,
) error {
	p := newPrinter(out)

	if cond := FindCondition(sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady); cond == nil {
		p.println("! this backup has not been reconciled yet; the run will start once it is")
	} else if cond.Status != metav1.ConditionTrue {
		p.printf("! this backup is not ready (%s); the run will start once it is\n", cond.Reason)
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := triggerBackup(ctx, c, sb, at); err != nil {
		return err
	}
	p.printf("triggered backup of scheduledbackup/%s\n", sb.Name)

	if !waitForIt {
		p.printf("  the operator will create job/%s\n", backup.ManualJobName(sb, at))
		return p.Err()
	}
	if err := p.Err(); err != nil {
		return err
	}

	jobName, err := awaitTrigger(ctx, c, sb, at, timeout)
	if err != nil {
		return err
	}
	p.printf("  started job/%s\n", jobName)
	if err := p.Err(); err != nil {
		return err
	}

	return awaitJob(ctx, c, cs, out, sb, jobName, follow, timeout)
}

func triggerBackup(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, at time.Time) error {
	hold, err := currentRestoreHold(ctx, c, sb)
	if err != nil {
		return err
	}
	if hold != "" {
		return fmt.Errorf("%w: scheduledbackup/%s is held: %s", ErrRestoreRunning, sb.Name, hold)
	}
	patch := client.MergeFrom(sb.DeepCopy())
	if sb.Annotations == nil {
		sb.Annotations = map[string]string{}
	}
	sb.Annotations[b2backupv1.AnnotationTriggerAt] = at.Format(time.RFC3339)
	return c.Patch(ctx, sb, patch)
}

func awaitTrigger(
	ctx context.Context,
	c client.Client,
	sb *b2backupv1.ScheduledBackup,
	at time.Time,
	timeout time.Duration,
) (string, error) {
	key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}

	var jobName string
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			var latest b2backupv1.ScheduledBackup
			if err := c.Get(ctx, key, &latest); err != nil {
				return false, err
			}
			acked := latest.Status.LastTriggerTime
			switch {
			case acked == nil || acked.Time.Before(at):
				return false, nil
			case acked.After(at):
				return false, fmt.Errorf(
					"%w: the operator has moved on to a trigger at %s, so this one (%s) will never start; "+
						"look for a clock ahead of yours or a hand-written %s annotation",
					ErrTriggerSkipped, acked.UTC().Format(time.RFC3339),
					at.Format(time.RFC3339), b2backupv1.AnnotationTriggerAt)
			}
			jobName = latest.Status.LastTriggerJob
			return true, nil
		})
	if err != nil {
		return "", fmt.Errorf(
			"waiting for the operator to acknowledge the trigger at %s (status.lastTriggerTime never matched it): %w",
			at.Format(time.RFC3339), err)
	}

	if jobName == "" {
		return "", fmt.Errorf(
			"%w: a backup is already running and concurrencyPolicy is Forbid; "+
				"wait for it to finish, or use `b2bk status %s` to see it", ErrTriggerSkipped, sb.Name)
	}
	return jobName, nil
}

func awaitJob(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	out io.Writer,
	sb *b2backupv1.ScheduledBackup,
	jobName string,
	follow bool,
	timeout time.Duration,
) error {
	key := types.NamespacedName{Namespace: sb.Namespace, Name: jobName}

	if follow {
		if err := followJobLogs(ctx, c, cs, out, key, timeout); err != nil {
			return err
		}
	}

	job, err := waitForJob(ctx, c, key, timeout)
	if err != nil {
		return err
	}

	p := newPrinter(out)
	if job.Status.Failed > 0 {
		p.printf("job/%s failed\n", jobName)
		if err := p.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: see `b2bk logs %s`", ErrBackupFailed, sb.Name)
	}
	p.printf("job/%s succeeded\n", jobName)
	return p.Err()
}

func waitForJob(
	ctx context.Context,
	c client.Client,
	key types.NamespacedName,
	timeout time.Duration,
) (*batchv1.Job, error) {
	var job batchv1.Job
	deleted := false
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, key, &job); err != nil {
				if !apierrors.IsNotFound(err) {
					return false, err
				}
				deleted = true
				return true, nil
			}
			return !jobIsRunning(&job), nil
		})
	if err != nil {
		return nil, fmt.Errorf("waiting for job/%s: %w", key.Name, err)
	}
	if deleted {
		return nil, fmt.Errorf(
			"%w: job/%s is gone, so its outcome is unknown; `b2bk cancel` or the operator may have removed it",
			errJobDeleted, key.Name)
	}
	return &job, nil
}

func followJobLogs(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	out io.Writer,
	key types.NamespacedName,
	timeout time.Duration,
) error {
	var job batchv1.Job
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, key, &job); err != nil {
				if apierrors.IsNotFound(err) {
					return false, fmt.Errorf("%w: job/%s is gone", errJobDeleted, key.Name)
				}
				return false, err
			}
			pods, err := podsForJob(ctx, c, &job)
			if err != nil {
				return false, err
			}
			for i := range pods {
				if pods[i].Status.Phase != corev1.PodPending {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for the backup pod: %w", err)
	}

	return streamJobLogs(ctx, c, cs, out, &job, corev1.PodLogOptions{Follow: true})
}
