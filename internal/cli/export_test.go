package cli

import (
	"bytes"
	"context"
	"io"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

// RestoreExec stands in for the restic container of a one-off restore pod.
type RestoreExec func(ctx context.Context, argv []string, stdout io.Writer) error

func (e RestoreExec) exec(ctx context.Context, argv []string, stdout io.Writer) error {
	return e(ctx, argv, stdout)
}

// NewTestFactory returns a Factory reading through c, whose one-off pods are
// whatever attach returns for their options instead of real Jobs.
func NewTestFactory(c client.Client, errOut io.Writer, attach func(runner.Options) (RestoreExec, error)) *Factory {
	return NewTestFactoryIn("", c, io.Discard, errOut, attach)
}

// NewTestFactoryIn is NewTestFactory acting on namespace ns and writing to out.
func NewTestFactoryIn(
	ns string, c client.Client, out, errOut io.Writer, attach func(runner.Options) (RestoreExec, error),
) *Factory {
	f := &Factory{
		ConfigFlags: genericclioptions.NewConfigFlags(true),
		Streams:     genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: out, ErrOut: errOut},
	}
	if ns != "" {
		*f.ConfigFlags.Namespace = ns
	}
	f.client = c
	f.once.client.Do(func() {})
	f.attachHook = func(_ context.Context, _ runner.Subject, opts runner.Options,
		fn func(restorePod, string) error) error {
		pod, err := attach(opts)
		if err != nil {
			return err
		}
		return fn(pod, "test-restore-job")
	}
	return f
}

// RestoreToNewPVC runs b2bk restore --to-new-pvc name for the latest snapshot.
func RestoreToNewPVC(ctx context.Context, f *Factory, sb *b2backupv1.ScheduledBackup, name string) error {
	c, err := f.Client()
	if err != nil {
		return err
	}
	return restoreToNewPVC(ctx, f, c, sb, &restoreOptions{toNewPVC: name, snapshot: "latest"})
}

// Doctor runs b2bk doctor arg.
var Doctor = runDoctor

// Status runs b2bk status arg.
var Status = runStatus

// PrintRepositories prints b2bk get repositories.
var PrintRepositories = printRepositories

// PrintScheduledBackups prints b2bk get backups.
var PrintScheduledBackups = printScheduledBackups

// RefuseActive is the check an overwriting restore makes for anything still
// running against the backup's claims or database.
var RefuseActive = refuseActive

// RotateWriteFailure explains a failed write of a rotated password.
var RotateWriteFailure = rotateWriteFailure

// KeyCommands are the commands rotate-password prints for the old key.
var KeyCommands = keyCommands

// Untar unpacks a restic dump tar stream into dest.
var Untar = untar

// IsOperatorImage reports whether an image is the operator's backup image.
var IsOperatorImage = isOperatorImage

// IsUpstreamImage reports whether an image is the one the operator's is forked from.
var IsUpstreamImage = isUpstreamImage

// RestoreArgs builds the restic restore of the whole of snapPath, or of sub
// under it, into target.
func RestoreArgs(snapPath, sub, target string, include []string, del, dryRun bool) ([]string, error) {
	o := &restoreOptions{include: include, delete: del, dryRun: dryRun}
	return o.restoreArgs(resticSnapshot{ID: "snap", Paths: []string{snapPath}},
		restoreStep{snapPath: snapPath, sub: sub}, restoreSource{paths: []string{snapPath}}, target)
}

// Execute runs b2bk with args against f.
func Execute(f *Factory, args ...string) error {
	cmd := newRoot(f, "b2bk")
	cmd.SetArgs(args)
	return cmd.Execute()
}

// RunWith makes f hand the one-off Jobs of b2bk commands that stream logs to
// run instead of starting them.
func RunWith(f *Factory, run func(runner.Subject, runner.Options) error) {
	f.runHook = func(_ context.Context, s runner.Subject, opts runner.Options) error { return run(s, opts) }
}
