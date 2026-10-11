package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	defaultResticTimeout = time.Hour

	cleanupTimeout = 30 * time.Second
)

// ErrNoBackupForRepository means no backup references the repository, so there is no
// CronJob to derive a Job from.
var ErrNoBackupForRepository = errors.New("no ScheduledBackup uses this repository")

// Runner returns a Runner backed by this Factory's clients.
func (f *Factory) Runner() (*runner.Runner, error) {
	c, err := f.Client()
	if err != nil {
		return nil, err
	}
	cs, err := f.Clientset()
	if err != nil {
		return nil, err
	}
	cfg, err := f.RESTConfig()
	if err != nil {
		return nil, err
	}
	return &runner.Runner{
		Client:          c,
		Clientset:       cs,
		RESTConfig:      cfg,
		PodStartTimeout: f.podStartTimeout,
		Progress:        f.Streams.ErrOut,
	}, nil
}

func resolveRunTarget(
	ctx context.Context, c client.Client, namespace, arg string,
) (*b2backupv1.ScheduledBackup, *b2backupv1.Repository, error) {
	target, err := Resolve(ctx, c, namespace, arg)
	if err != nil {
		return nil, nil, err
	}

	if target.Kind == TargetScheduledBackup {
		repo, err := RepositoryFor(ctx, c, target.ScheduledBackup)
		if err != nil {
			return nil, nil, err
		}
		return target.ScheduledBackup, repo, nil
	}

	repo := target.Repository
	var backups b2backupv1.ScheduledBackupList
	if err := c.List(ctx, &backups, client.InNamespace(repo.Namespace)); err != nil {
		return nil, nil, err
	}
	for i := range backups.Items {
		if backups.Items[i].Spec.RepositoryRef.Name == repo.Name {
			return &backups.Items[i], repo, nil
		}
	}
	return nil, nil, fmt.Errorf(
		"%w: repository/%s has no ScheduledBackup to borrow credentials and placement from",
		ErrNoBackupForRepository, repo.Name)
}

func sourceTags(sb *b2backupv1.ScheduledBackup) []string {
	seen := map[string]bool{}
	var tags []string
	for i := range sb.Spec.Sources {
		tag := sb.Spec.Sources[i].EffectiveTag()
		if tag != "" && !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	return tags
}

func resticCommand(args ...string) []string {
	return append([]string{"restic"}, args...)
}

func runRestic(
	ctx context.Context,
	f *Factory,
	arg string,
	purpose string,
	build func(sb *b2backupv1.ScheduledBackup, repo *b2backupv1.Repository) ([]string, error),
	opts runner.Options,
) error {
	c, err := f.Client()
	if err != nil {
		return err
	}
	ns, err := f.Namespace()
	if err != nil {
		return err
	}

	sb, repo, err := resolveRunTarget(ctx, c, ns, arg)
	if err != nil {
		return err
	}

	command, err := build(sb, repo)
	if err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}

	opts.Command = command
	opts.Purpose = purpose
	return run.Run(ctx, sb, opts, f.Streams.Out, defaultResticTimeout)
}
