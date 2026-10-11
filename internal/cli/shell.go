package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/remotecommand"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/kube"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

func newShellCommand(f *Factory) *cobra.Command {
	var opts runner.Options
	var shell string

	cmd := &cobra.Command{
		Use:     "shell <name>",
		Aliases: []string{"sh"},
		Short:   "Open a shell with restic configured",
		GroupID: GroupEscape,
		Long: `Start a pod from the backup image and drop into a shell.

restic, runitor, ts and dumpdb are on PATH, and RESTIC_REPOSITORY and
RESTIC_PASSWORD are already set, so restic works with no arguments:

    restic snapshots
    restic mount /mnt

The pod is deleted when the shell exits. Nothing from the app is mounted unless
--mount-data is passed, and the shared restic cache is replaced with scratch
space unless --mount-cache is passed, so this cannot interfere with a running
backup.`,
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

			run, err := f.Runner()
			if err != nil {
				return err
			}

			opts.Purpose = "shell"
			return openShell(cmd.Context(), run, f, sb, opts, shell)
		},
	}

	cmd.Flags().BoolVar(&opts.MountData, "mount-data", false,
		"Mount the backup's source volume, writable")
	cmd.Flags().BoolVar(&opts.MountCache, "mount-cache", false,
		"Mount the shared restic cache instead of scratch space")
	cmd.Flags().StringVar(&opts.Image, "image", "", "Override the backup image")
	cmd.Flags().BoolVar(&opts.Keep, "keep", false, "Leave the pod running after the shell exits")
	cmd.Flags().StringVar(&shell, "shell", "sh", "Shell to start")
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}

func openShell(
	ctx context.Context,
	run *runner.Runner,
	f *Factory,
	sb *b2backupv1.ScheduledBackup,
	opts runner.Options,
	shell string,
) error {
	stdin, ok := f.Streams.In.(*os.File)
	interactive := ok && f.Interactive()

	return run.Attach(ctx, sb, opts, defaultResticTimeout, func(pod *corev1.Pod) error {
		p := newPrinter(f.Streams.ErrOut)
		p.printf("connected to pod/%s\n", pod.Name)
		if err := p.Err(); err != nil {
			return err
		}

		exec := func() error {
			execCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			return kube.Exec(execCtx, run.RESTConfig, run.Clientset, kube.ExecOptions{
				Namespace: pod.Namespace,
				Pod:       pod.Name,
				Container: runner.ContainerName,
				Command:   []string{shell},
				Stdin:     f.Streams.In,
				Stdout:    f.Streams.Out,
				Stderr:    f.Streams.ErrOut,
				TTY:       interactive,
				SizeQueue: sizeQueueFor(execCtx, interactive, stdin),
			})
		}

		if !interactive {
			return exec()
		}
		return kube.WithRawTerminal(stdin.Fd(), exec)
	})
}

func sizeQueueFor(ctx context.Context, interactive bool, stdin *os.File) remotecommand.TerminalSizeQueue {
	if !interactive {
		return nil
	}
	return kube.NewSizeQueue(ctx, stdin.Fd())
}

func newExecCommand(f *Factory) *cobra.Command {
	var opts runner.Options

	cmd := &cobra.Command{
		Use:     "exec <name> -- <command>...",
		Short:   "Run one restic command",
		GroupID: GroupEscape,
		Long: `Run a single command in the backup's environment and print its output.

Everything after -- is passed through, so this reaches restic subcommands the
CLI does not wrap:

    b2bk exec web-files -- restic find --tag=files 'config.php'
    b2bk exec web-files -- restic cat config`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 1 {
				return fmt.Errorf("separate the command with --, e.g. b2bk exec %s -- restic snapshots", args[0])
			}
			command := args[1:]

			return runRestic(cmd.Context(), f, args[0], "exec",
				func(*b2backupv1.ScheduledBackup, *b2backupv1.Repository) ([]string, error) {
					return command, nil
				},
				opts)
		},
	}

	cmd.Flags().BoolVar(&opts.MountData, "mount-data", false,
		"Mount the backup's source volume, writable")
	cmd.Flags().BoolVar(&opts.MountCache, "mount-cache", false,
		"Mount the shared restic cache instead of scratch space")
	cmd.Flags().StringVar(&opts.Image, "image", "", "Override the backup image")
	f.AddPodStartTimeoutFlag(cmd)
	return cmd
}
