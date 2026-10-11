package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

// ErrRestoreRunning means another restore that overwrites live data holds the
// ScheduledBackup, or one that shares its claims or database.
var ErrRestoreRunning = errors.New("another restore is running")

// restoreHold is what one overwriting restore holds: the ScheduledBackup it
// restores, and every other one in the namespace that backs up the same claims
// or database, each carrying AnnotationRestoreLock. While it is set the
// operator keeps their CronJobs suspended and starts no triggered backup.
type restoreHold struct {
	c    client.Client
	id   string
	plan *b2backupv1.ScheduledBackup
	keys []types.NamespacedName

	// claims and database are what makes another ScheduledBackup share the
	// restore's target: see sharingBackups.
	claims   []string
	database bool

	// targets are the AnnotationRestorePartial entries this restore writes.
	targets []string
	// clears are the entries a complete restore of the whole source makes up
	// for: those of the claims its paths are on, never one mounted under them,
	// which the source may exclude.
	clears []string
	// full is true once the restore is known to write the whole of each target,
	// so its success makes up for an earlier partial restore of it.
	full bool

	// added are the targets this restore recorded before its first write, on
	// each held ScheduledBackup, that no earlier restore had recorded.
	added map[types.NamespacedName][]string
}

func restoreLockID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("b2bk pid %d on %s at %s", os.Getpid(), host, time.Now().UTC().Format(time.RFC3339))
}

func partialTargets(sb *b2backupv1.ScheduledBackup) []string { return backup.PartialTargets(sb) }

func setPartialTargets(sb *b2backupv1.ScheduledBackup, targets []string) {
	slices.Sort(targets)
	targets = slices.Compact(targets)
	if len(targets) == 0 {
		delete(sb.Annotations, b2backupv1.AnnotationRestorePartial)
		return
	}
	raw, err := json.Marshal(targets)
	if err != nil {
		return
	}
	if sb.Annotations == nil {
		sb.Annotations = map[string]string{}
	}
	sb.Annotations[b2backupv1.AnnotationRestorePartial] = string(raw)
}

// restoredClaims returns the claims a restore of paths writes to: the one each
// path is on, and every claim mounted under one of them.
func restoredClaims(sb *b2backupv1.ScheduledBackup, paths []string) []string {
	var claims []string
	for _, v := range claimVolumes(sb, paths) {
		if !slices.Contains(claims, v.ExistingClaim) {
			claims = append(claims, v.ExistingClaim)
		}
	}
	return claims
}

type claimVolume struct {
	b2backupv1.VolumeSpec
	rel    string
	nested bool
}

func claimVolumes(sb *b2backupv1.ScheduledBackup, paths []string) []claimVolume {
	var out []claimVolume
	for _, p := range paths {
		if v := backup.MountFor(sb, p); v != nil {
			rel := "/" + strings.TrimLeft(strings.TrimPrefix(p, v.EffectiveMountPath()), "/")
			out = append(out, claimVolume{VolumeSpec: *v, rel: rel})
		}
		for _, v := range sb.Spec.Volumes {
			if m := v.EffectiveMountPath(); m != p && under(m, p) {
				out = append(out, claimVolume{VolumeSpec: v, rel: "/", nested: true})
			}
		}
	}
	return out
}

// claimTargets are the AnnotationRestorePartial entries for files restored to
// paths: the claim each is on and where on it, and every claim mounted under one.
func claimTargets(sb *b2backupv1.ScheduledBackup, paths []string) []string {
	volumes := claimVolumes(sb, paths)
	targets := make([]string, 0, len(volumes))
	for _, v := range volumes {
		targets = append(targets, "pvc/"+v.ExistingClaim+":"+v.rel)
	}
	return targets
}

// directClaimTargets are the claimTargets entries of the claims the paths are
// on, leaving out claims mounted under them.
func directClaimTargets(sb *b2backupv1.ScheduledBackup, paths []string) []string {
	var targets []string
	for _, p := range paths {
		if shadowed(sb, p) {
			continue
		}
		for _, v := range claimVolumes(sb, []string{p}) {
			if !v.nested {
				targets = append(targets, "pvc/"+v.ExistingClaim+":"+v.rel)
			}
		}
	}
	return targets
}

// shadowed reports whether another claim or the scratch space is mounted under
// p, so a restore of p never writes the part of p's own claim beneath it.
func shadowed(sb *b2backupv1.ScheduledBackup, p string) bool {
	for _, v := range sb.Spec.Volumes {
		if m := v.EffectiveMountPath(); m != p && under(m, p) {
			return true
		}
	}
	return sb.Spec.Scratch != nil && under(path.Clean(sb.Spec.Scratch.MountPath), p)
}

// databaseHost is the host a database restore writes to when the spec names
// one: spec.database.host for mariadb, else DB_HOST from spec.env. It is ""
// when the credentials Secret names it.
func databaseHost(sb *b2backupv1.ScheduledBackup) string {
	db := sb.Spec.Database
	if db.Engine == b2backupv1.DatabaseEngineMariaDB {
		return db.Host
	}
	return sb.Spec.Env["DB_HOST"]
}

// databaseKey is the AnnotationRestorePartial entry for a database restore.
func databaseKey(sb *b2backupv1.ScheduledBackup, database string) string {
	db := sb.Spec.Database
	switch {
	case database != "":
	case db.Engine == b2backupv1.DatabaseEngineMariaDB && db.Name != "":
		database = db.Name
	case sb.Spec.Env["DB_DATABASE"] != "":
		database = sb.Spec.Env["DB_DATABASE"]
	default:
		database = "dbname"
	}
	host := databaseHost(sb)
	if host == "" {
		host = "from-secret"
	}
	return fmt.Sprintf("db/%s/%s/%s/%s", db.Engine, db.EffectiveSecretName(), host, database)
}

// sharingBackups returns the ScheduledBackups in sb's namespace that back up
// one of these claims, or, with database set, a database through the same
// credentials Secret, sb first.
func sharingBackups(
	ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, claims []string, database bool,
) ([]b2backupv1.ScheduledBackup, error) {
	var list b2backupv1.ScheduledBackupList
	if err := c.List(ctx, &list, client.InNamespace(sb.Namespace)); err != nil {
		return nil, err
	}
	out := []b2backupv1.ScheduledBackup{*sb}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == sb.Name {
			continue
		}
		shares := false
		for _, v := range other.Spec.Volumes {
			shares = shares || slices.Contains(claims, v.ExistingClaim)
		}
		if database && other.Spec.Database != nil && sb.Spec.Database != nil &&
			other.Spec.Database.Engine == sb.Spec.Database.Engine &&
			other.Spec.Database.EffectiveSecretName() == sb.Spec.Database.EffectiveSecretName() &&
			(databaseHost(other) == "" || databaseHost(sb) == "" || databaseHost(other) == databaseHost(sb)) {
			shares = true
		}
		if shares {
			out = append(out, *other)
		}
	}
	return out, nil
}

// holdForRestore refuses while a backup or another restore of any of backups
// runs, then takes them all for this restore. Each write carries the
// resourceVersion it read, so of two restores racing for one only one wins; one
// that loses lets go of what it took.
func holdForRestore(
	ctx context.Context, c client.Client, plan *b2backupv1.ScheduledBackup, claims []string, database bool,
	targets, clears []string,
) (*restoreHold, error) {
	backups, err := sharingBackups(ctx, c, plan, claims, database)
	if err != nil {
		return nil, err
	}
	for i := range backups {
		if err := refuseActive(ctx, c, &backups[i], ""); err != nil {
			return nil, err
		}
	}
	h := &restoreHold{
		c: c, id: restoreLockID(), plan: plan.DeepCopy(), claims: claims, database: database,
		targets: targets, clears: clears,
	}
	for i := range backups {
		key := types.NamespacedName{Namespace: backups[i].Namespace, Name: backups[i].Name}
		if err := h.take(ctx, key); err != nil {
			_, releaseErr := h.release(outcomeUntouched)
			return nil, errors.Join(err, releaseErr)
		}
		h.keys = append(h.keys, key)
	}
	return h, nil
}

func (h *restoreHold) take(ctx context.Context, key types.NamespacedName) error {
	var latest b2backupv1.ScheduledBackup
	if err := h.c.Get(ctx, key, &latest); err != nil {
		return err
	}
	if holder := latest.Annotations[b2backupv1.AnnotationRestoreLock]; holder != "" {
		return fmt.Errorf("%w: scheduledbackup/%s is held by %s; if that restore and its Job are gone "+
			"(kubectl get jobs -n %s -l %s), release it with kubectl annotate -n %s scheduledbackup/%s %s-; "+
			"what it was writing stays held until you check it and run b2bk resume %s",
			ErrRestoreRunning, key.Name, holder, key.Namespace, runner.LabelPurpose, key.Namespace, key.Name,
			b2backupv1.AnnotationRestoreLock, key.Name)
	}
	patch := client.MergeFromWithOptions(latest.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[b2backupv1.AnnotationRestoreLock] = h.id
	if err := h.c.Patch(ctx, &latest, patch); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("%w: scheduledbackup/%s changed while it was being taken; try again",
				ErrRestoreRunning, key.Name)
		}
		return fmt.Errorf("holding scheduledbackup/%s for the restore: %w", key.Name, err)
	}
	return nil
}

type restoreOutcome int

const (
	outcomeUntouched restoreOutcome = iota
	outcomeComplete
	outcomePartial
)

// release lets every held ScheduledBackup go and records the outcome: a
// partial restore adds its targets to AnnotationRestorePartial, which keeps the
// schedules held, and a complete restore of the whole of each target removes
// them. It returns the entries still recorded.
func (h *restoreHold) release(outcome restoreOutcome) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
	defer cancel()

	var left []string
	var errs []error
	for _, key := range h.keys {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var latest b2backupv1.ScheduledBackup
			if err := h.c.Get(ctx, key, &latest); err != nil {
				return err
			}
			if holder := latest.Annotations[b2backupv1.AnnotationRestoreLock]; holder != h.id {
				return fmt.Errorf("%w: scheduledbackup/%s is now held by %q", ErrRestoreRunning, key.Name, holder)
			}
			patch := client.MergeFromWithOptions(latest.DeepCopy(), client.MergeFromWithOptimisticLock{})
			delete(latest.Annotations, b2backupv1.AnnotationRestoreLock)
			targets := partialTargets(&latest)
			switch outcome {
			case outcomePartial:
				targets = append(targets, h.targets...)
			default:
				added := h.added[key]
				targets = slices.DeleteFunc(targets, func(t string) bool {
					return slices.Contains(added, t) || (outcome == outcomeComplete && h.full && slices.Contains(h.clears, t))
				})
			}
			setPartialTargets(&latest, targets)
			if err := h.c.Patch(ctx, &latest, patch); err != nil {
				return err
			}
			left = append(left, partialTargets(&latest)...)
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	slices.Sort(left)
	return slices.Compact(left), errors.Join(errs...)
}

// finish releases the hold after a restore that returned err, having written
// to the target or not, and says what still holds the schedules.
func (h *restoreHold) finish(f *Factory, err error, wrote bool) error {
	if h == nil {
		return err
	}
	outcome := outcomeUntouched
	switch {
	case wrote && err != nil:
		outcome = outcomePartial
	case wrote:
		outcome = outcomeComplete
	}
	left, releaseErr := h.release(outcome)
	names := make([]string, 0, len(h.keys))
	for _, key := range h.keys {
		names = append(names, "scheduledbackup/"+key.Name)
	}
	p := newPrinter(f.Streams.ErrOut)
	if len(left) > 0 {
		p.printf("! %s stay held, with the CronJob suspended and no backup triggered:\n", strings.Join(names, ", "))
		p.printf("! %s may hold a partial restore, which the next backup would save\n", strings.Join(left, ", "))
		p.printf("! and retention could then keep over good snapshots. Restore it in full again,\n")
		p.printf("! or check it and run: b2bk resume %s\n", h.plan.Name)
	}
	if releaseErr != nil {
		p.printf("! could not release %s after the restore: %v\n", strings.Join(names, ", "), releaseErr)
		p.printf("! check the %s annotation with: b2bk status %s\n", b2backupv1.AnnotationRestoreLock, h.plan.Name)
	}
	return errors.Join(err, p.Err())
}

// readyToWrite is checked in the restore pod, just before the first write: the
// ScheduledBackup is as the restore was planned from and the operator rendered
// it, every held CronJob is suspended, and no backup or other restore started.
func (h *restoreHold) readyToWrite(ctx context.Context, ownJob string) error {
	var latest b2backupv1.ScheduledBackup
	if err := h.c.Get(ctx, client.ObjectKeyFromObject(h.plan), &latest); err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(latest.Spec, h.plan.Spec) {
		return fmt.Errorf("%w: scheduledbackup/%s changed after the restore was planned; nothing was written, "+
			"try again", ErrNotCurrent, h.plan.Name)
	}
	if err := requireCurrentBackup(ctx, h.c, &latest); err != nil {
		return err
	}
	sharing, err := sharingBackups(ctx, h.c, &latest, h.claims, h.database)
	if err != nil {
		return err
	}
	for i := range sharing {
		if !slices.Contains(h.keys, client.ObjectKeyFromObject(&sharing[i])) {
			return fmt.Errorf("%w: scheduledbackup/%s came to share the restore's target after it was held; "+
				"nothing was written, try again", ErrNotCurrent, sharing[i].Name)
		}
	}
	for _, key := range h.keys {
		var sb b2backupv1.ScheduledBackup
		if err := h.c.Get(ctx, key, &sb); err != nil {
			return err
		}
		var cj batchv1.CronJob
		switch err := h.c.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: backup.CronJobName(&sb)}, &cj); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return err
		case cj.Spec.Suspend == nil || !*cj.Spec.Suspend:
			return fmt.Errorf("%w: the operator has not suspended cronjob/%s yet, so a backup could start "+
				"during the restore; nothing was written, try again", ErrNotCurrent, cj.Name)
		}
		if err := refuseActive(ctx, h.c, &sb, ownJob); err != nil {
			return err
		}
	}
	return h.recordTargets(ctx)
}

// recordTargets adds the restore's targets to AnnotationRestorePartial before
// its first write, so a restore that dies mid-write, or whose lock is removed
// by hand, still leaves its targets held.
func (h *restoreHold) recordTargets(ctx context.Context) error {
	h.added = map[types.NamespacedName][]string{}
	for _, key := range h.keys {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var latest b2backupv1.ScheduledBackup
			if err := h.c.Get(ctx, key, &latest); err != nil {
				return err
			}
			if holder := latest.Annotations[b2backupv1.AnnotationRestoreLock]; holder != h.id {
				return fmt.Errorf("%w: scheduledbackup/%s is now held by %q", ErrRestoreRunning, key.Name, holder)
			}
			existing := partialTargets(&latest)
			var added []string
			for _, t := range h.targets {
				if !slices.Contains(existing, t) {
					added = append(added, t)
				}
			}
			patch := client.MergeFromWithOptions(latest.DeepCopy(), client.MergeFromWithOptimisticLock{})
			setPartialTargets(&latest, append(existing, added...))
			if err := h.c.Patch(ctx, &latest, patch); err != nil {
				return err
			}
			h.added[key] = added
			return nil
		})
		if err != nil {
			return fmt.Errorf("recording what the restore writes on scheduledbackup/%s; nothing was written: %w",
				key.Name, err)
		}
	}
	return nil
}

func refuseActive(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, ownJob string) error {
	job, err := activeBackup(ctx, c, sb, ownJob)
	if err != nil || job == "" {
		return err
	}
	return fmt.Errorf("%w: job/%s is running for scheduledbackup/%s; wait for it with b2bk wait %s, "+
		"or stop it with b2bk cancel %s", ErrBackupRunning, job, sb.Name, sb.Name, sb.Name)
}

// writingPurposes are the b2bk Jobs that write to a backup's claims or database.
var writingPurposes = []string{purposeRestore, purposeRestoreDB}

// activeBackup returns a Job of this ScheduledBackup that has not finished,
// other than ownJob: a backup the CronJob started, a triggered one, or a b2bk
// Job restoring into its claims or database. A b2bk Job from before Jobs
// recorded their purpose counts as a restore.
func activeBackup(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, ownJob string) (string, error) {
	owners := []types.UID{sb.UID}
	var cj batchv1.CronJob
	switch err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}, &cj); {
	case err == nil:
		owners = append(owners, cj.UID)
	case !apierrors.IsNotFound(err):
		return "", err
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(sb.Namespace)); err != nil {
		return "", err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == ownJob || backup.JobFinished(job) {
			continue
		}
		if owner := metav1.GetControllerOf(job); owner != nil && slices.Contains(owners, owner.UID) {
			return job.Name, nil
		}
		if job.Labels[labelManagedBy] != runner.ManagedByValue || !ownedBy(job, sb.UID) {
			continue
		}
		if purpose, ok := job.Labels[runner.LabelPurpose]; !ok || slices.Contains(writingPurposes, purpose) {
			return job.Name, nil
		}
	}
	return "", nil
}

func ownedBy(obj metav1.Object, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}
