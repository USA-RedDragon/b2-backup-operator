package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

var errUnknownSourceTags = errors.New("snapshot tags are not known")

type snapshotsOptions struct {
	allTags bool
	host    string
	hostSet bool
	asJSON  bool
	latest  int
}

func newSnapshotsCommand(f *Factory) *cobra.Command {
	var o snapshotsOptions

	cmd := &cobra.Command{
		Use:     "snapshots <name>",
		Aliases: []string{"snaps"},
		Short:   "List a backup's snapshots",
		GroupID: GroupSnapshots,
		Long: `List the snapshots a ScheduledBackup has written.

One repository can serve several backups, so the listing is filtered to this
backup's own source tags. A spec.script backup has no sources to filter by, so
it needs --all-tags.

The host filter defaults to the host this backup records its snapshots under:
spec.resticHost, or the namespace when that is unset; --host="" lifts it.

--json prints restic's JSON and nothing else on stdout: a plain list of
snapshots, since it passes --group-by= over the image's RESTIC_GROUP_BY.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.hostSet = cmd.Flags().Changed("host")

			return runRestic(cmd.Context(), f, args[0], "snapshots",
				func(sb *b2backupv1.ScheduledBackup, _ *b2backupv1.Repository) ([]string, error) {
					return snapshotsCommand(sb, &o)
				},
				runner.Options{})
		},
	}

	cmd.Flags().BoolVar(&o.allTags, "all-tags", false, "Show every snapshot in the repository")
	cmd.Flags().StringVar(&o.host, "host", "", "Filter by snapshot host (defaults to spec.resticHost, or the namespace)")
	cmd.Flags().BoolVar(&o.asJSON, "json", false, "Emit restic's JSON output, a plain list, on stdout alone")
	cmd.Flags().IntVar(&o.latest, "latest", 0, "Show only the newest N snapshots")
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}

func snapshotsCommand(sb *b2backupv1.ScheduledBackup, o *snapshotsOptions) ([]string, error) {
	host := o.host
	if !o.hostSet {
		host = sb.EffectiveResticHost()
	}

	argv := []string{"snapshots"}
	if !o.allTags {
		tagArgs, err := snapshotTagArgs(sb)
		if err != nil {
			return nil, err
		}
		argv = append(argv, tagArgs...)
	}
	if host != "" {
		argv = append(argv, "--host="+host)
	}
	if o.asJSON {
		argv = append(argv, jsonFlag, "--group-by=")
	}
	if o.latest > 0 {
		argv = append(argv, fmt.Sprintf("--latest=%d", o.latest))
	}
	return resticCommand(argv...), nil
}

func snapshotTagArgs(sb *b2backupv1.ScheduledBackup) ([]string, error) {
	tags := sourceTags(sb)
	if len(tags) == 0 {
		return nil, fmt.Errorf(
			"%w: scheduledbackup/%s has no spec.sources, so its snapshots cannot be told apart from "+
				"anything else in the repository; pass --all-tags to list all of it",
			errUnknownSourceTags, sb.Name)
	}
	argv := make([]string, 0, len(tags))
	for _, tag := range tags {
		argv = append(argv, "--tag="+tag)
	}
	return argv, nil
}
