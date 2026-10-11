package cli

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

// Errors returned when a destructive command finds the operator behind.
var (
	ErrNotCurrent  = errors.New("the operator has not rendered this backup's current spec")
	ErrTagConflict = errors.New("the backup is in a tag conflict")

	ErrRepositoryNotUsable = errors.New("the repository is not Ready as its own")
)

const (
	labelManagedBy    = "app.kubernetes.io/managed-by"
	operatorManagedBy = "b2-backup-operator"
)

// requireCurrent refuses a command that forgets or overwrites unless the
// operator has rendered the backup's CronJob from the ScheduledBackup and
// Repository as they are now, and nothing else in the namespace forgets under
// its tags. A prune scoped from a newer spec, or a restore laid out from one,
// would otherwise act on a schedule the operator never accepted: one it holds
// in a tag conflict, or a CronJob it could not update and suspended.
func requireCurrent(
	ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, repo *b2backupv1.Repository,
) error {
	wait := fmt.Sprintf("check b2bk status %s and wait for the operator to reconcile it", sb.Name)
	if sb.Status.ObservedGeneration != sb.Generation {
		return fmt.Errorf("%w: scheduledbackup/%s is at generation %d and the operator has seen %d; %s",
			ErrNotCurrent, sb.Name, sb.Generation, sb.Status.ObservedGeneration, wait)
	}
	cond := apimeta.FindStatusCondition(sb.Status.Conditions, b2backupv1.ScheduledBackupConditionReady)
	switch {
	case cond == nil:
		return fmt.Errorf("%w: scheduledbackup/%s has no Ready condition; %s", ErrNotCurrent, sb.Name, wait)
	case cond.Reason == "TagConflict":
		return fmt.Errorf("%w: scheduledbackup/%s's forget would reach another backup's snapshots: %s",
			ErrTagConflict, sb.Name, cond.Message)
	case cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != sb.Generation:
		return fmt.Errorf("%w: scheduledbackup/%s is not Ready at generation %d (%s: %s); %s",
			ErrNotCurrent, sb.Name, sb.Generation, cond.Reason, cond.Message, wait)
	}

	var cj batchv1.CronJob
	key := types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}
	if err := c.Get(ctx, key, &cj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: cronjob/%s does not exist; %s", ErrNotCurrent, key.Name, wait)
		}
		return err
	}
	if !metav1.IsControlledBy(&cj, sb) || cj.Annotations[backup.AnnotationSpecHash] == "" {
		return fmt.Errorf("%w: cronjob/%s is not one the operator rendered for scheduledbackup/%s; %s",
			ErrNotCurrent, cj.Name, sb.Name, wait)
	}
	if got, want := cj.Annotations[backup.AnnotationRenderedFrom], backup.RenderedFrom(sb, repo); got != want {
		return fmt.Errorf("%w: cronjob/%s was rendered from %q, not the current %s; %s",
			ErrNotCurrent, cj.Name, got, want, wait)
	}
	if scope, ok := backup.ScopeFrom(cj.Spec.JobTemplate.Annotations); !ok || !scope.Equal(backup.ScopeOf(sb)) {
		return fmt.Errorf("%w: cronjob/%s forgets under another host or tags than scheduledbackup/%s; %s",
			ErrNotCurrent, cj.Name, sb.Name, wait)
	}

	var backups b2backupv1.ScheduledBackupList
	if err := c.List(ctx, &backups, client.InNamespace(sb.Namespace)); err != nil {
		return err
	}
	managed := client.MatchingLabels{labelManagedBy: operatorManagedBy}
	var cronJobs batchv1.CronJobList
	if err := c.List(ctx, &cronJobs, client.InNamespace(sb.Namespace), managed); err != nil {
		return err
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(sb.Namespace), managed); err != nil {
		return err
	}
	if conflict := backup.FindConflict(sb, backups.Items, cronJobs.Items, jobs.Items); conflict != nil {
		return fmt.Errorf("%w: %s also backs up into repository/%s, under %s", ErrTagConflict,
			conflict.With, sb.Spec.RepositoryRef.Name, conflict.Under)
	}
	return nil
}

// requireOwnRepository refuses a command that changes the repository behind
// the Repository's credentials Secret unless the Repository is Ready and that
// Secret is the one it recorded writing, so the command cannot reach another
// Repository's restic repository.
func requireOwnRepository(repo *b2backupv1.Repository) error {
	if why := controller.RepositoryNotOwn(repo); why != "" {
		return fmt.Errorf("%w: repository/%s %s", ErrRepositoryNotUsable, repo.Name, why)
	}
	cond := apimeta.FindStatusCondition(repo.Status.Conditions, b2backupv1.RepositoryConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return fmt.Errorf("%w: repository/%s is not Ready; check b2bk status repo/%s",
			ErrRepositoryNotUsable, repo.Name, repo.Name)
	}
	return nil
}

func requireCurrentBackup(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup) error {
	repo, err := RepositoryFor(ctx, c, sb)
	if err != nil {
		return err
	}
	return requireCurrent(ctx, c, sb, repo)
}
