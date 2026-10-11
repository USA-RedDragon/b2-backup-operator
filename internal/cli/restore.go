package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// Errors returned by the restore command.
var (
	ErrNoTarget       = errors.New("no restore target specified")
	ErrNotConfirmed   = errors.New("not confirmed")
	ErrNoRestoreDB    = errors.New("the backup image has no restoredb")
	ErrNoDatabase     = errors.New("this backup has no database")
	ErrNoSourceVolume = errors.New("this backup has no source volume")
)

const restoreMountPath = "/restore"

type restoreOptions struct {
	snapshot string

	inPlace    bool
	toNewPVC   string
	toDatabase bool
	toDir      string

	size    string
	path    string
	include []string
	exclude []string
	delete  bool
	dryRun  bool
	yes     bool
}

func (o *restoreOptions) targets() []string {
	var set []string
	if o.inPlace {
		set = append(set, "--in-place")
	}
	if o.toNewPVC != "" {
		set = append(set, "--to-new-pvc")
	}
	if o.toDatabase {
		set = append(set, "--to-database")
	}
	if o.toDir != "" {
		set = append(set, "--to")
	}
	return set
}

func (o *restoreOptions) unsupportedForDir() []string {
	var set []string
	if len(o.include) > 0 {
		set = append(set, "--include")
	}
	if len(o.exclude) > 0 {
		set = append(set, "--exclude")
	}
	if o.delete {
		set = append(set, "--delete")
	}
	return set
}

func newRestoreCommand(f *Factory) *cobra.Command {
	var o restoreOptions

	cmd := &cobra.Command{
		Use:     "restore <name>",
		Short:   "Restore a snapshot",
		GroupID: GroupSnapshots,
		Long: `Restore a snapshot to one of four places.

  --to DIR            download the files to this machine
  --to-new-pvc NAME   restore into a fresh claim, to inspect before committing
  --in-place          write back over the backup's source volume
  --to-database       stream the dump back into the database

--to downloads with restic dump, which selects with --path alone, so
--include, --exclude and --delete are refused rather than ignored there.

With no target and a terminal, b2bk asks. Without a terminal it lists the
targets and exits, so a script never restores somewhere it did not name.

--in-place and --to-database overwrite live data and ask for confirmation.`,
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
			sb, _, err := resolveRunTarget(cmd.Context(), c, ns, args[0])
			if err != nil {
				return err
			}
			return runRestore(cmd.Context(), f, c, sb, &o)
		},
	}

	cmd.Flags().StringVar(&o.snapshot, "snapshot", "latest", "Snapshot to restore")
	cmd.Flags().BoolVar(&o.inPlace, "in-place", false, "Restore over the backup's source volume")
	cmd.Flags().StringVar(&o.toNewPVC, "to-new-pvc", "", "Restore into a new claim of this name")
	cmd.Flags().BoolVar(&o.toDatabase, "to-database", false, "Restore the dump into the database")
	cmd.Flags().StringVar(&o.toDir, "to", "", "Download the files into this directory")
	cmd.Flags().StringVar(&o.size, "size", "", "Size of the new claim (default: the source claim's size)")
	cmd.Flags().StringVar(&o.path, "path", "", "Restore only this path from the snapshot")
	cmd.Flags().StringArrayVar(&o.include, "include", nil, "Restore only paths matching this pattern")
	cmd.Flags().StringArrayVar(&o.exclude, "exclude", nil, "Skip paths matching this pattern")
	cmd.Flags().BoolVar(&o.delete, "delete", false,
		"Remove files at the target that are not in the snapshot")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "Report what would be restored")
	cmd.Flags().BoolVar(&o.yes, "yes", false, "Skip the confirmation prompt")
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}

func runRestore(
	ctx context.Context,
	f *Factory,
	c client.Client,
	sb *b2backupv1.ScheduledBackup,
	o *restoreOptions,
) error {
	if set := o.targets(); len(set) > 1 {
		return fmt.Errorf("choose one restore target, got %s", strings.Join(set, " and "))
	} else if len(set) == 0 {
		if err := chooseTarget(f, sb, o); err != nil {
			return err
		}
	}

	switch {
	case o.toDir != "":
		return restoreToDir(ctx, f, sb, o)
	case o.toNewPVC != "":
		return restoreToNewPVC(ctx, f, c, sb, o)
	case o.toDatabase:
		return restoreToDatabase(ctx, f, sb, o)
	default:
		return restoreInPlace(ctx, f, c, sb, o)
	}
}

func availableTargets(sb *b2backupv1.ScheduledBackup) []string {
	targets := []string{"--to DIR (download to this machine)"}
	if sb.Spec.Volume != nil {
		targets = append(targets,
			"--to-new-pvc NAME (a fresh claim, for inspection)",
			fmt.Sprintf("--in-place (overwrite pvc/%s)", sb.Spec.Volume.ExistingClaim))
	}
	if sb.Spec.Database != nil {
		targets = append(targets, "--to-database (overwrite the live database)")
	}
	return targets
}

func chooseTarget(f *Factory, sb *b2backupv1.ScheduledBackup, o *restoreOptions) error {
	targets := availableTargets(sb)

	if !f.Interactive() {
		p := newPrinter(f.Streams.ErrOut)
		p.printf("scheduledbackup/%s can restore to:\n", sb.Name)
		for _, t := range targets {
			p.printf("  %s\n", t)
		}
		if err := p.Err(); err != nil {
			return err
		}
		return ErrNoTarget
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("Where should scheduledbackup/%s be restored?\n", sb.Name)
	for i, t := range targets {
		p.printf("  %d) %s\n", i+1, t)
	}
	p.print("Choose [1]: ")
	if err := p.Err(); err != nil {
		return err
	}

	line, err := f.Stdin().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	choice := 1
	if s := strings.TrimSpace(line); s != "" {
		if _, err := fmt.Sscanf(s, "%d", &choice); err != nil || choice < 1 || choice > len(targets) {
			return fmt.Errorf("%w: %q is not one of 1-%d", ErrNoTarget, s, len(targets))
		}
	}

	switch {
	case strings.HasPrefix(targets[choice-1], "--to "):
		return fmt.Errorf("%w: pass --to DIR with the directory to download into", ErrNoTarget)
	case strings.HasPrefix(targets[choice-1], "--to-new-pvc"):
		return fmt.Errorf("%w: pass --to-new-pvc NAME with a name for the new claim", ErrNoTarget)
	case strings.HasPrefix(targets[choice-1], "--in-place"):
		o.inPlace = true
	default:
		o.toDatabase = true
	}
	return nil
}

func confirm(f *Factory, skip bool, what, name string) error {
	if skip {
		return nil
	}
	if !f.Interactive() {
		return fmt.Errorf(
			"%w: this overwrites %s, and there is no terminal to confirm on; pass --yes to proceed",
			ErrNotConfirmed, what)
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("This overwrites %s.\n", what)
	p.printf("Type %q to confirm: ", name)
	if err := p.Err(); err != nil {
		return err
	}

	line, err := f.Stdin().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) != name {
		return fmt.Errorf("%w: %s was not changed", ErrNotConfirmed, what)
	}
	return nil
}

func (o *restoreOptions) dumpPath() string {
	if o.path == "" {
		return "/"
	}
	return o.path
}

func (o *restoreOptions) resticDumpArgs() []string {
	return []string{"dump", "--archive=tar", o.snapshot, o.dumpPath()}
}

func (o *restoreOptions) resticListArgs() []string {
	argv := []string{"ls", "--long", "--recursive", o.snapshot}
	if o.path != "" {
		argv = append(argv, o.path)
	}
	return argv
}

func (o *restoreOptions) resticRestoreArgs(target string) []string {
	argv := []string{"restore", o.snapshot, "--target=" + target}
	if o.path != "" {
		argv = append(argv, "--include="+o.path)
	}
	for _, p := range o.include {
		argv = append(argv, "--include="+p)
	}
	for _, p := range o.exclude {
		argv = append(argv, "--exclude="+p)
	}
	if o.delete {
		argv = append(argv, "--delete")
	}
	if o.dryRun {
		argv = append(argv, "--dry-run", "--verbose")
	}
	return argv
}
