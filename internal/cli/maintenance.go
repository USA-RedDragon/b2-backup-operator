package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
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
all, which is safe only when nothing is running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if removeAll {
				if err := confirm(f, yes, "every lock on the repository, including live ones", args[0]); err != nil {
					return err
				}
			}
			return runRestic(cmd.Context(), f, args[0], "unlock",
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
		Use:     "check <name>",
		Short:   "Verify repository integrity",
		GroupID: GroupMaintenance,
		Long: `Check the repository's structure.

By default only metadata is verified, which is fast. --read-data-subset also
re-reads a share of the actual data, which is the only way to detect bit rot
but costs a full download of that share.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestic(cmd.Context(), f, args[0], "check",
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
writing into the same repository keeps its own retention.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !dryRun {
				if err := confirm(f, yes, "snapshots outside the retention policy, permanently", args[0]); err != nil {
					return err
				}
			}
			return runRestic(cmd.Context(), f, args[0], "prune",
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
		return nil, fmt.Errorf("scheduledbackup/%s is in a tag conflict, so its forget would reach another "+
			"backup's snapshots: %s", sb.Name, cond.Message)
	}
	tags := backup.SourceTags(sb.Spec.Sources)
	if len(tags) == 0 {
		return nil, fmt.Errorf("scheduledbackup/%s runs a script and has no source tags to scope forget to; "+
			"run restic forget with --tag yourself through b2bk exec %s", sb.Name, sb.Name)
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
		Use:     "stats <name>",
		Short:   "Show repository size",
		GroupID: GroupMaintenance,
		Args:    cobra.ExactArgs(1),
		Long: `Show restic's own view of the repository size.

The operator's measurement in ` + "`b2bk status`" + ` is the size of the files
in B2; this is what restic thinks, which differs because of deduplication,
compression, and files restic has deleted that B2 has only hidden so far.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(statsModes, mode) {
				return fmt.Errorf("%w %q, expected one of: %s",
					errUnknownStatsMode, mode, strings.Join(statsModes, "|"))
			}
			return runRestic(cmd.Context(), f, args[0], "stats",
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
