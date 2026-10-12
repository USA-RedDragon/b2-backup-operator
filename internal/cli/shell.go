package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/remotecommand"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/kube"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

func newShellCommand(f *Factory) *cobra.Command {
	var opts runner.Options
	var shell string

	cmd := &cobra.Command{
		Use:     "shell [NAME]",
		Aliases: []string{"sh"},
		Short:   "Open a shell with restic configured",
		GroupID: GroupEscape,
		Long: `Start a pod from the backup image and drop into a shell.

restic, ts and dumpdb are on PATH, and RESTIC_REPOSITORY and
RESTIC_PASSWORD are already set, so restic works with no arguments:

    restic snapshots
    restic mount /mnt

The pod is deleted when the shell exits. Nothing from the app is mounted unless
--mount-data is passed, and the shared restic cache is replaced with scratch
space unless --mount-cache is passed, so this cannot interfere with a running
backup.

With no NAME, it uses the namespace's only ScheduledBackup, or its only
Repository when it has no ScheduledBackup.`,
		Args:              argCount(0, 1, "name"),
		ValidArgsFunction: f.completeName(anyKind),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			arg, err := f.nameArg(cmd, args, anyKind)
			if err != nil {
				return err
			}
			sb, repo, err := resolveSubject(cmd.Context(), f, c, arg, true, &opts)
			if err != nil {
				return err
			}

			run, err := f.Runner()
			if err != nil {
				return err
			}

			opts.Purpose = "shell"
			return openShell(cmd.Context(), run, f, subjectOf(sb, repo), opts, shell)
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
	subject runner.Subject,
	opts runner.Options,
	shell string,
) error {
	stdin, ok := f.Streams.In.(*os.File)
	interactive := ok && f.Interactive()

	return run.Attach(ctx, subject, opts, defaultResticTimeout, func(pod *corev1.Pod) error {
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
				Command:   shellArgv(subject, shell, interactive, os.Getenv("NO_COLOR")),
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

// shellArgv starts shell, without the operator's default
// RESTIC_PROGRESS_FPS when on a terminal: restic redraws its status there by
// itself, and the default would slow that to one line every ten seconds.
func shellArgv(subject runner.Subject, shell string, interactive bool, noColor string) []string {
	var env []string
	if interactive && subject.Backup != nil {
		if _, set := subject.Backup.Spec.Env[backup.EnvProgressFPS]; !set {
			env = append(env, "-u", backup.EnvProgressFPS)
		}
	}
	if noColor != "" {
		env = append(env, "NO_COLOR="+noColor)
	}
	if len(env) == 0 {
		return []string{shell}
	}
	return append(append([]string{"env"}, env...), shell)
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
		Use:     "exec [NAME] -- <command>...",
		Short:   "Run one restic command",
		GroupID: GroupEscape,
		Long: `Run a single command in the backup's environment and print its output.

Everything after -- is passed through, so this reaches restic subcommands the
CLI does not wrap:

    b2bk exec web-files -- restic find --tag=files 'config.php'
    b2bk exec web-files -- restic cat config

With no NAME before --, it uses the namespace's only ScheduledBackup, or its
only Repository when it has no ScheduledBackup.`,
		Args:              execArgs,
		ValidArgsFunction: f.completeName(anyKind),
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			command := args[dash:]
			arg, err := f.nameArg(cmd, args[:dash], anyKind)
			if err != nil {
				return err
			}

			return runRestic(cmd.Context(), f, arg, "exec",
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
