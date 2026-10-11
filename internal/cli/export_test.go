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
	f := &Factory{
		ConfigFlags: genericclioptions.NewConfigFlags(true),
		Streams:     genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: io.Discard, ErrOut: errOut},
	}
	f.client = c
	f.once.client.Do(func() {})
	f.attachHook = func(_ context.Context, _ *b2backupv1.ScheduledBackup, opts runner.Options,
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
