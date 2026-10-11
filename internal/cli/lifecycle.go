package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newSuspendCommand(f *Factory) *cobra.Command {
	return newSuspendResumeCommand(f, true)
}

func newResumeCommand(f *Factory) *cobra.Command {
	return newSuspendResumeCommand(f, false)
}

func newSuspendResumeCommand(f *Factory, suspend bool) *cobra.Command {
	verb, past := "resume", "resumed"
	long := `Clear spec.suspend so the operator reconciles the resource again.`
	if suspend {
		verb, past = "suspend", "suspended"
		long = `Set spec.suspend.

Suspending a ScheduledBackup keeps its CronJob but stops it firing. Suspending a
Repository stops the operator reconciling it at all.`
	}

	cmd := &cobra.Command{
		Use:     verb + " <name>",
		Short:   capitalize(verb) + " a repository or scheduled backup",
		Long:    long,
		GroupID: GroupLifecycle,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			return runSetSuspend(cmd.Context(), c, f.Streams.Out, ns, args[0], suspend, past)
		},
	}
	return cmd
}

func runSetSuspend(
	ctx context.Context,
	c client.Client,
	out io.Writer,
	namespace, arg string,
	suspend bool,
	past string,
) error {
	target, err := Resolve(ctx, c, namespace, arg)
	if err != nil {
		return err
	}

	var obj client.Object
	var current *bool
	if target.Kind == TargetRepository {
		obj = target.Repository
		current = &target.Repository.Spec.Suspend
	} else {
		obj = target.ScheduledBackup
		current = &target.ScheduledBackup.Spec.Suspend
	}

	p := newPrinter(out)
	label := fmt.Sprintf("%s/%s", target.Kind, target.Name())
	if *current == suspend {
		p.printf("%s is already %s\n", label, past)
		return p.Err()
	}

	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	*current = suspend
	if err := c.Patch(ctx, obj, patch); err != nil {
		return err
	}

	p.printf("%s %s\n", label, past)
	return p.Err()
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
