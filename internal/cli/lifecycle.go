package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func newSuspendCommand(f *Factory) *cobra.Command {
	return newSuspendResumeCommand(f, true)
}

func newResumeCommand(f *Factory) *cobra.Command {
	return newSuspendResumeCommand(f, false)
}

func newSuspendResumeCommand(f *Factory, suspend bool) *cobra.Command {
	verb, past, use, argCheck := "resume", "resumed", "resume NAME",
		f.requiredName(anyKind, "can release a ScheduledBackup held after a failed restore")
	long := `Clear spec.suspend so the operator reconciles the resource again.

It also releases a ScheduledBackup held after a failed restore, so it needs a
name even when the namespace has only one.`
	if suspend {
		verb, past, use, argCheck = "suspend", "suspended", "suspend [NAME]", argCount(0, 1, "name")
		long = `Set spec.suspend.

Suspending a ScheduledBackup keeps its CronJob but stops it firing. Suspending a
Repository stops the operator reconciling it at all.

With no NAME, it suspends the namespace's only ScheduledBackup, or its only
Repository when it has no ScheduledBackup.`
	}

	cmd := &cobra.Command{
		Use:     use,
		Short:   capitalize(verb) + " a repository or scheduled backup",
		Long:    long,
		GroupID: GroupLifecycle,
		Args:    argCheck,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			arg, err := f.nameArg(cmd, args, anyKind)
			if err != nil {
				return err
			}
			return runSetSuspend(cmd.Context(), c, f.Streams.Out, ns, arg, suspend, past)
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
	var partial []string
	if sb := target.ScheduledBackup; target.Kind == TargetScheduledBackup && !suspend {
		if holder := sb.Annotations[b2backupv1.AnnotationRestoreLock]; holder != "" {
			return fmt.Errorf("%w: %s is held by %s; it resumes when that restore ends; if that restore and its "+
				"Job are gone, release it with kubectl annotate -n %s scheduledbackup/%s %s-", ErrRestoreRunning,
				label, holder, sb.Namespace, sb.Name, b2backupv1.AnnotationRestoreLock)
		}
		partial = partialTargets(sb)
	}
	if *current == suspend && len(partial) == 0 {
		p.printf("%s is already %s\n", label, past)
		return errors.Join(p.Err(), reportStillHeld(ctx, c, p, target, label, suspend))
	}

	patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
	*current = suspend
	if len(partial) > 0 {
		delete(target.ScheduledBackup.Annotations, b2backupv1.AnnotationRestorePartial)
	}
	if err := c.Patch(ctx, obj, patch); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("%s changed while it was being updated; try again: %w", label, err)
		}
		return err
	}

	if len(partial) > 0 {
		p.printf("%s no longer held for a partial restore of %s\n", label, strings.Join(partial, ", "))
		released, err := releasePartial(ctx, c, target.ScheduledBackup, partial)
		for _, name := range released {
			p.printf("scheduledbackup/%s no longer held for it either\n", name)
		}
		if err != nil {
			return errors.Join(err, p.Err())
		}
	}
	p.printf("%s %s\n", label, past)
	return errors.Join(p.Err(), reportStillHeld(ctx, c, p, target, label, suspend))
}

// reportStillHeld says, after a resume, what restore still holds the backup.
func reportStillHeld(ctx context.Context, c client.Client, p *printer, target *Target, label string, suspend bool) error {
	sb := target.ScheduledBackup
	if target.Kind != TargetScheduledBackup || suspend {
		return nil
	}
	var latest b2backupv1.ScheduledBackup
	if err := c.Get(ctx, client.ObjectKeyFromObject(sb), &latest); err != nil {
		return err
	}
	hold, err := currentRestoreHold(ctx, c, &latest)
	if err != nil {
		return err
	}
	if hold != "" {
		p.printf("! %s is still held: %s\n", label, hold)
	}
	return p.Err()
}

// releasePartial removes entries from AnnotationRestorePartial on every other
// ScheduledBackup in sb's namespace, where the restore that wrote them recorded
// them too, except one a running restore holds: that restore may be writing
// them again.
func releasePartial(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, entries []string) ([]string, error) {
	var list b2backupv1.ScheduledBackupList
	if err := c.List(ctx, &list, client.InNamespace(sb.Namespace)); err != nil {
		return nil, err
	}
	var released []string
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == sb.Name || other.Annotations[b2backupv1.AnnotationRestoreLock] != "" ||
			!slices.ContainsFunc(partialTargets(other), func(e string) bool { return slices.Contains(entries, e) }) {
			continue
		}
		locked := false
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var latest b2backupv1.ScheduledBackup
			if err := c.Get(ctx, client.ObjectKeyFromObject(other), &latest); err != nil {
				return err
			}
			if locked = latest.Annotations[b2backupv1.AnnotationRestoreLock] != ""; locked {
				return nil
			}
			patch := client.MergeFromWithOptions(latest.DeepCopy(), client.MergeFromWithOptimisticLock{})
			setPartialTargets(&latest, slices.DeleteFunc(partialTargets(&latest), func(e string) bool {
				return slices.Contains(entries, e)
			}))
			return c.Patch(ctx, &latest, patch)
		})
		if err != nil {
			return released, err
		}
		if !locked {
			released = append(released, other.Name)
		}
	}
	return released, nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
