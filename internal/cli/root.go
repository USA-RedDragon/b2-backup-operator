// Package cli implements b2bk, which doubles as the kubectl-b2bk plugin.
package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// Command groups, in the order they appear in help output.
const (
	GroupInspect     = "inspect"
	GroupRun         = "run"
	GroupSnapshots   = "snapshots"
	GroupMaintenance = "maintenance"
	GroupLifecycle   = "lifecycle"
	GroupEscape      = "escape"
	GroupDevelopment = "development"
)

const pluginPrefix = "kubectl-"

// DisplayName returns how the command should refer to itself, which differs when it is
// invoked as a kubectl plugin.
func DisplayName(argv0 string) string {
	base := filepath.Base(argv0)
	base = strings.TrimSuffix(base, ".exe")
	if name, ok := strings.CutPrefix(base, pluginPrefix); ok {
		return "kubectl " + strings.ReplaceAll(name, "_", "-")
	}
	return "b2bk"
}

// New returns the root command.
func New(streams genericiooptions.IOStreams, argv0 string) *cobra.Command {
	return newRoot(NewFactory(streams), argv0)
}

func newRoot(f *Factory, argv0 string) *cobra.Command {
	streams := f.Streams
	display := DisplayName(argv0)
	name := display
	if after, ok := strings.CutPrefix(display, "kubectl "); ok {
		name = after
	}

	cmd := &cobra.Command{
		Use:   name,
		Short: "Operate b2-backup-operator backups",
		Long: `b2bk drives the backups that b2-backup-operator schedules.

It triggers backups on demand, explains why one is not running, browses and
restores snapshots, and drops you into a shell with restic already configured.

On a terminal, b2bk with no command opens a full-screen dashboard of the same
actions; see b2bk tui --help.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Annotations: map[string]string{
			cobra.CommandDisplayNameAnnotation: display,
		},
		Args: unknownCommand,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !f.canDrawTUI() {
				return cmd.Help()
			}
			return runTUI(cmd, f)
		},
	}
	addTUIFlags(cmd)

	cmd.SetIn(streams.In)
	cmd.SetOut(streams.Out)
	cmd.SetErr(streams.ErrOut)

	f.ConfigFlags.AddFlags(cmd.PersistentFlags())
	f.registerConnectionFlagCompletions(cmd)
	cmd.CompletionOptions.SetDefaultShellCompDirective(cobra.ShellCompDirectiveNoFileComp)

	subcommands := []*cobra.Command{
		newBackupCommand(f),
		newCancelCommand(f),
		newWaitCommand(f),
		newDoctorCommand(f),
		newGetCommand(f),
		newLogsCommand(f),
		newStatusCommand(f),
		newSuspendCommand(f),
		newResumeCommand(f),
		newReinitCommand(f),
		newRotatePasswordCommand(f),
		newEnvCommand(f),
		newSnapshotsCommand(f),
		newRestoreCommand(f),
		newUnlockCommand(f),
		newCheckCommand(f),
		newPruneCommand(f),
		newStatsCommand(f),
		newShellCommand(f),
		newExecCommand(f),
		newRenderCommand(f),
		newValidateCommand(f),
		newMigrateCommand(f),
		newParityCommand(f),
		newVersionCommand(f),
		newTUICommand(f),
	}

	used := make(map[string]bool, len(subcommands))
	for _, sub := range subcommands {
		if sub.GroupID != "" {
			used[sub.GroupID] = true
		}
	}
	for _, g := range groupOrder() {
		if used[g.ID] {
			cmd.AddGroup(g)
		}
	}

	cmd.AddCommand(subcommands...)

	return cmd
}

func groupOrder() []*cobra.Group {
	return []*cobra.Group{
		{ID: GroupInspect, Title: "Inspect:"},
		{ID: GroupRun, Title: "Run:"},
		{ID: GroupSnapshots, Title: "Snapshots and restore:"},
		{ID: GroupMaintenance, Title: "Maintenance:"},
		{ID: GroupLifecycle, Title: "Lifecycle:"},
		{ID: GroupEscape, Title: "Escape hatch:"},
		{ID: GroupDevelopment, Title: "Development:"},
	}
}

// unknownCommand refuses arguments to the bare command the way cobra refuses an
// unknown command, with its suggestions, since a runnable root otherwise loses them.
func unknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return fmt.Errorf("%s\n\nRun '%s --help' for usage", msg, cmd.CommandPath())
}
