package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

var errUnknownStatsMode = errors.New("unsupported stats mode")

var statsModes = []string{"restore-size", "files-by-contents", "blobs-per-file", "raw-data"}

func newUnlockCommand(f *Factory) *cobra.Command {
	var removeAll, yes bool

	cmd := &cobra.Command{
		Use:     "unlock <name>",
		Short:   "Remove a stale restic lock",
		GroupID: GroupMaintenance,
		Long: `Remove stale locks from the repository.

A backup killed mid-run leaves a lock behind, and every later run then fails
waiting for it. This is the usual fix.

Only locks restic considers stale are removed. Use --remove-all to remove them
all, which is safe only when nothing is running: it is refused while any Job of
a ScheduledBackup on the repository, or of the Repository itself, is running.`,
		Args: argCount(1, 1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if removeAll {
				if err := confirm(f, yes, "every lock on the repository, including live ones", args[0]); err != nil {
					return err
				}
			}
			var guard func(context.Context, client.Client, *b2backupv1.ScheduledBackup, *b2backupv1.Repository) error
			if removeAll {
				guard = func(
					ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, repo *b2backupv1.Repository,
				) error {
					if why := controller.RepositoryNotOwn(repo); why != "" {
						return fmt.Errorf("%w: repository/%s %s, so its locks may be another repository's",
							ErrRepositoryNotUsable, repo.Name, why)
					}
					return refuseRepositoryInUse(ctx, c, sb, repo)
				}
			}
			return runResticGuarded(cmd.Context(), f, args[0], "unlock", true, guard,
				func(*b2backupv1.ScheduledBackup, *b2backupv1.Repository) ([]string, error) {
					argv := []string{"unlock"}
					if removeAll {
						argv = append(argv, "--remove-all")
					}
					return resticCommand(argv...), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().BoolVar(&removeAll, "remove-all", false,
		"Remove every lock, not only stale ones. Only safe when no backup is running")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}

func newCheckCommand(f *Factory) *cobra.Command {
	var readData string

	cmd := &cobra.Command{
		Use:     "check [NAME]",
		Short:   "Verify repository integrity",
		GroupID: GroupMaintenance,
		Long: `Check the repository's structure.

By default only metadata is verified, which is fast. --read-data-subset also
re-reads a share of the actual data, which is the only way to detect bit rot
but costs a full download of that share.

With no NAME, it checks the namespace's only ScheduledBackup's repository, or
its only Repository when it has no ScheduledBackup.`,
		Args: argCount(0, 1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg, err := f.nameArg(cmd, args, anyKind)
			if err != nil {
				return err
			}
			return runRestic(cmd.Context(), f, arg, "check",
				func(*b2backupv1.ScheduledBackup, *b2backupv1.Repository) ([]string, error) {
					argv := []string{"check"}
					if readData != "" {
						argv = append(argv, "--read-data-subset="+readData)
					}
					return resticCommand(argv...), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().StringVar(&readData, "read-data-subset", "",
		`Also verify a share of the data, e.g. "10%" or "1/5"`)
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}

func newPruneCommand(f *Factory) *cobra.Command {
	var dryRun, yes bool

	cmd := &cobra.Command{
		Use:     "prune <name>",
		Short:   "Apply the retention policy and reclaim space",
		GroupID: GroupMaintenance,
		Long: `Run restic forget --prune using the ScheduledBackup's retention policy.

Backups already prune themselves after each run, so this is for reclaiming
space after a retention change rather than routine upkeep.

Like the scheduled forget, it only considers this backup's own snapshots: those
tagged with one of its sources' tags, on its host. Another ScheduledBackup
writing into the same repository keeps its own retention.

It runs only once the operator has rendered the backup's CronJob from the
ScheduledBackup and Repository as they are now and reports it Ready, and only
while nothing else in the namespace forgets under its tags: a ScheduledBackup,
a CronJob left without one, or a backup Job still running an older spec.`,
		Args: argCount(1, 1, "name"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				if err := confirm(f, yes, "snapshots outside the retention policy, permanently", args[0]); err != nil {
					return err
				}
			}
			return runResticGuarded(cmd.Context(), f, args[0], "prune", false, requireCurrent,
				func(sb *b2backupv1.ScheduledBackup, repo *b2backupv1.Repository) ([]string, error) {
					flags := retentionFlags(sb.Spec.Retention)
					if len(flags) == 0 {
						return nil, fmt.Errorf(
							"scheduledbackup/%s has no spec.retention, so there is nothing to forget",
							sb.Name)
					}

					argv, err := pruneArgs(sb, flags)
					if err != nil {
						return nil, err
					}
					if dryRun {
						argv = append(argv, "--dry-run")
					}
					return resticCommand(argv...), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be removed without removing it")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}

func pruneArgs(sb *b2backupv1.ScheduledBackup, keep []string) ([]string, error) {
	if cond := apimeta.FindStatusCondition(sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady); cond != nil &&
		cond.Reason == "TagConflict" {
		return nil, fmt.Errorf("%w: scheduledbackup/%s's forget would reach another backup's snapshots: %s",
			ErrTagConflict, sb.Name, cond.Message)
	}
	tags := backup.SourceTags(sb.Spec.Sources)
	if len(tags) == 0 {
		return nil, fmt.Errorf("scheduledbackup/%s runs a script and has no source tags to scope forget to; "+
			"run restic forget with --tag yourself through b2bk exec sb/%s", sb.Name, sb.Name)
	}
	argv := []string{"forget", "--prune", "--host=" + sb.EffectiveResticHost(), backup.ForgetGroupFlag}
	for _, tag := range tags {
		argv = append(argv, "--tag="+tag)
	}
	return append(argv, keep...), nil
}

func retentionFlags(r *b2backupv1.Retention) []string {
	if r == nil {
		return nil
	}
	var flags []string
	for _, f := range []struct {
		name  string
		value *int32
	}{
		{"last", r.Last},
		{"hourly", r.Hourly},
		{"daily", r.Daily},
		{"weekly", r.Weekly},
		{"monthly", r.Monthly},
		{"yearly", r.Yearly},
	} {
		if f.value != nil {
			flags = append(flags, fmt.Sprintf("--keep-%s=%d", f.name, *f.value))
		}
	}
	return flags
}

func newStatsCommand(f *Factory) *cobra.Command {
	var mode string

	cmd := &cobra.Command{
		Use:     "stats [NAME]",
		Short:   "Show repository size",
		GroupID: GroupMaintenance,
		Args:    argCount(0, 1, "name"),
		Long: `Show restic's own view of the repository size.

The operator's measurement in ` + "`b2bk status`" + ` is the size of the files
in B2; this is what restic thinks, which differs because of deduplication,
compression, and files restic has deleted that B2 has only hidden so far.

With no NAME, it uses the namespace's only ScheduledBackup, or its only
Repository when it has no ScheduledBackup.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(statsModes, mode) {
				return fmt.Errorf("%w %q, expected one of: %s",
					errUnknownStatsMode, mode, strings.Join(statsModes, "|"))
			}
			arg, err := f.nameArg(cmd, args, anyKind)
			if err != nil {
				return err
			}
			return runRestic(cmd.Context(), f, arg, "stats",
				func(*b2backupv1.ScheduledBackup, *b2backupv1.Repository) ([]string, error) {
					return resticCommand("stats", "--mode="+mode), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().StringVar(&mode, "mode", statsModes[0],
		"restic stats mode: "+strings.Join(statsModes, ", "))
	_ = cmd.RegisterFlagCompletionFunc("mode",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return statsModes, cobra.ShellCompDirectiveNoFileComp
		})
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}

// refuseRepositoryInUse refuses while any Job may hold a live lock on the
// repository: one of a ScheduledBackup that writes into it, its CronJob's, a
// b2bk Job of either, the Repository's own, or an operator Job whose recorded
// forget scope names it.
func refuseRepositoryInUse(
	ctx context.Context, c client.Client, _ *b2backupv1.ScheduledBackup, repo *b2backupv1.Repository,
) error {
	owners := map[types.UID]bool{repo.UID: true}
	var backups b2backupv1.ScheduledBackupList
	if err := c.List(ctx, &backups, client.InNamespace(repo.Namespace)); err != nil {
		return err
	}
	var cronJobs batchv1.CronJobList
	if err := c.List(ctx, &cronJobs, client.InNamespace(repo.Namespace)); err != nil {
		return err
	}
	names := map[string]bool{}
	for i := range backups.Items {
		sb := &backups.Items[i]
		if sb.Spec.RepositoryRef.Name != repo.Name {
			continue
		}
		owners[sb.UID] = true
		names[sb.Name] = true
		for j := range cronJobs.Items {
			if cronJobs.Items[j].Name == backup.CronJobName(sb) {
				owners[cronJobs.Items[j].UID] = true
			}
		}
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(repo.Namespace)); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if backup.JobFinished(job) {
			continue
		}
		scope, ok := backup.ScopeFrom(job.Annotations)
		inUse := ok && scope.Repository == repo.Name
		for _, ref := range job.OwnerReferences {
			inUse = inUse || owners[ref.UID]
		}
		if inUse {
			work := &runningRestic{kind: kindJob, name: job.Name}
			if job.Labels[labelManagedBy] == runner.ManagedByValue {
				work.b2bk, work.purpose = true, job.Labels[runner.LabelPurpose]
			}
			return fmt.Errorf("%w: %s is running and may hold a live lock on repository/%s; wait for it, "+
				"or unlock without --remove-all", work.lockSentinel(), work, repo.Name)
		}
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(repo.Namespace)); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		inUse := pod.Labels[labelManagedBy] == operatorManagedBy && names[pod.Labels["app.kubernetes.io/name"]]
		if pod.Labels[labelManagedBy] == runner.ManagedByValue {
			inUse = inUse || pod.Labels[runner.LabelRepository] == repo.Name
			owner := pod.Labels[runner.LabelScheduledBackup]
			for name := range names {
				inUse = inUse || owner == name || (owner == "" && strings.HasPrefix(podJob(pod), name+"-b2bk-"))
			}
		}
		if inUse {
			work := &runningRestic{kind: kindPod, name: pod.Name, terminating: pod.DeletionTimestamp != nil}
			if pod.Labels[labelManagedBy] == runner.ManagedByValue {
				work.b2bk, work.purpose = true, pod.Labels[runner.LabelPurpose]
			}
			return fmt.Errorf("%w: %s is still running restic and may hold a live lock on repository/%s; wait "+
				"for it, or unlock without --remove-all. If it stays Terminating because its node is down, make sure "+
				"the node is really off, then kubectl delete pod -n %s %s --force --grace-period=0",
				work.lockSentinel(), work, repo.Name, pod.Namespace, pod.Name)
		}
	}
	return nil
}
