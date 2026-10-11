package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/kube"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	purposeRestore  = "restore"
	purposeDownload = "download"
)

type runnerPod struct {
	run    *runner.Runner
	pod    *corev1.Pod
	stderr io.Writer
}

func (p runnerPod) exec(ctx context.Context, argv []string, stdout io.Writer) error {
	return p.run.Exec(ctx, p.pod, kube.ExecOptions{Command: argv, Stdout: stdout, Stderr: p.stderr})
}

func restoreToDir(
	ctx context.Context, f *Factory, sb *b2backupv1.ScheduledBackup, o *restoreOptions,
) error {
	if set := o.unsupportedForDir(); len(set) > 0 {
		return fmt.Errorf(
			"%s cannot be used with --to: a download runs restic dump, which takes only --path",
			strings.Join(set, " and "))
	}
	src, err := o.filesSource(sb)
	if err != nil {
		return err
	}

	dest, err := filepath.Abs(o.toDir)
	if err != nil {
		return err
	}
	if !o.dryRun {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}
	return run.Attach(ctx, sb, runner.Options{Purpose: purposeDownload}, defaultResticTimeout,
		func(pod *corev1.Pod) error {
			return o.download(ctx, runnerPod{run: run, pod: pod, stderr: f.Streams.ErrOut}, src, dest,
				f.Streams.Out, f.Streams.ErrOut)
		})
}

func (o *restoreOptions) download(
	ctx context.Context, pod restorePod, src restoreSource, dest string, out, errOut io.Writer,
) error {
	snap, kinds, err := o.resolve(ctx, pod, src)
	if err != nil {
		return err
	}
	steps, err := o.steps(snap, kinds)
	if err != nil {
		return err
	}

	total := 0
	for _, step := range steps {
		p := newPrinter(errOut)
		root := filepath.Join(dest, filepath.FromSlash(step.snapPath))
		if o.dryRun {
			p.printf("listing %s:%s%s; nothing is written to %s\n", snap.short(), step.snapPath, step.sub, dest)
			if err := p.Err(); err != nil {
				return err
			}
			if err := pod.exec(ctx, resticCommand(listArgs(snap, step)...), out); err != nil {
				return err
			}
			continue
		}
		p.printf("downloading %s:%s%s into %s\n", snap.short(), step.snapPath, step.sub, root)
		if err := p.Err(); err != nil {
			return err
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}

		if step.file {
			if err := downloadFile(ctx, pod, snap, step, root); err != nil {
				return err
			}
			total++
			continue
		}
		if step.sub != "" {
			var listing bytes.Buffer
			argv := []string{"ls", jsonFlag, snap.ID + ":" + step.snapPath, step.sub}
			if err := pod.exec(ctx, resticCommand(argv...), &listing); err != nil {
				return err
			}
			switch nodeType(listing.Bytes(), step.sub) {
			case "":
				return fmt.Errorf("%s is not in snapshot %s:%s", step.sub, snap.short(), step.snapPath)
			case fileNode:
				if err := downloadFile(ctx, pod, snap, step, root); err != nil {
					return err
				}
				total++
				continue
			}
		}

		n, err := untarFrom(root, func(w io.Writer) error {
			return pod.exec(ctx, resticCommand(dumpArgs(snap, step)...), w)
		})
		total += n
		if err != nil {
			return err
		}
	}
	if o.dryRun {
		return nil
	}
	done := newPrinter(errOut)
	done.printf("restored %d files into %s\n", total, dest)
	return done.Err()
}

func downloadFile(ctx context.Context, pod restorePod, snap resticSnapshot, step restoreStep, root string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	name := strings.TrimPrefix(step.sub, "/")
	if err := r.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	file, err := r.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := pod.exec(ctx, resticCommand("dump", snap.ID+":"+step.snapPath, step.sub), file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func untarFrom(dest string, produce func(w io.Writer) error) (int, error) {
	reader, writer := io.Pipe()

	errCh := make(chan error, 1)
	go func() {
		err := produce(writer)
		_ = writer.CloseWithError(err)
		errCh <- err
	}()

	n, untarErr := untar(reader, dest)
	_ = reader.CloseWithError(untarErr)
	produceErr := <-errCh
	if untarErr != nil {
		return n, untarErr
	}
	return n, produceErr
}

func untar(r io.Reader, dest string) (int, error) {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(r)
	count := 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}

		target := filepath.Join(dest, filepath.Clean("/"+header.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) &&
			target != filepath.Clean(dest) {
			return count, fmt.Errorf("refusing entry outside the target directory: %q", header.Name)
		}
		name, err := filepath.Rel(dest, target)
		if err != nil {
			return count, err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, os.FileMode(header.Mode)); err != nil {
				return count, err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			_ = root.Remove(name)
			if err := writeFile(root, name, os.FileMode(header.Mode), tr); err != nil {
				return count, err
			}
			count++
		case tar.TypeSymlink:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			_ = root.Remove(name)
			if err := root.Symlink(header.Linkname, name); err != nil {
				return count, err
			}
			count++
		}
	}
}

func writeFile(root *os.Root, path string, mode os.FileMode, r io.Reader) error {
	file, err := root.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, r); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func restoreToNewPVC(
	ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup, o *restoreOptions,
) error {
	src, err := o.filesSource(sb)
	if err != nil {
		return err
	}
	claim, err := sourceVolume(sb, src)
	if err != nil {
		return err
	}

	spec, err := newClaimSpec(ctx, c, sb, claim, o.size)
	if err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}
	var plan volumePlan
	err = run.Attach(ctx, sb, runner.Options{Purpose: purposeRestore}, defaultResticTimeout,
		func(pod *corev1.Pod) error {
			plan, err = o.planVolumes(ctx, runnerPod{run: run, pod: pod, stderr: f.Streams.ErrOut}, src,
				newClaimTarget(sb, claim, restoreMountPath))
			return err
		})
	if err != nil {
		return err
	}

	pvc := newRestoreClaim(sb, o.toNewPVC, spec)
	if err := c.Create(ctx, pvc); err != nil {
		return fmt.Errorf("creating pvc/%s: %w", o.toNewPVC, err)
	}

	size := spec.Resources.Requests[corev1.ResourceStorage]
	p := newPrinter(f.Streams.ErrOut)
	p.printf("created pvc/%s (%s), laid out like pvc/%s\n", pvc.Name, size.String(), claim.ExistingClaim)
	if err := p.Err(); err != nil {
		return err
	}

	opts := runner.Options{
		Purpose: purposeRestore,
		Restore: true,
		ExtraVolumes: []corev1.Volume{{
			Name: "restore-target",
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc.Name,
			},
		}},
		ExtraMounts: []corev1.VolumeMount{{Name: "restore-target", MountPath: restoreMountPath}},
	}
	err = run.Attach(ctx, sb, opts, defaultResticTimeout, func(pod *corev1.Pod) error {
		return o.runVolumes(ctx, runnerPod{run: run, pod: pod, stderr: f.Streams.ErrOut}, src, plan, f.Streams.Out)
	})
	if err != nil {
		return err
	}

	done := newPrinter(f.Streams.ErrOut)
	done.printf("restored into pvc/%s; the claim is ready to mount\n", pvc.Name)
	return done.Err()
}

// newRestoreClaim is the claim --to-new-pvc restores into. It owns nothing and
// nothing owns it, so it outlives the restore.
func newRestoreClaim(sb *b2backupv1.ScheduledBackup, name string, spec corev1.PersistentVolumeClaimSpec) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Namespace: sb.Namespace,
		Name:      name,
		Labels:    map[string]string{"app.kubernetes.io/managed-by": runner.ManagedByValue},
		Spec:      spec,
	}
}

func newClaimSpec(
	ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, claim *b2backupv1.VolumeSpec, override string,
) (corev1.PersistentVolumeClaimSpec, error) {
	spec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}

	var source corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: sb.Namespace, Name: claim.ExistingClaim}
	switch err := c.Get(ctx, key, &source); {
	case err == nil:
		if len(source.Spec.AccessModes) > 0 {
			spec.AccessModes = source.Spec.AccessModes
		}
		spec.StorageClassName = source.Spec.StorageClassName
	case override == "":
		return spec, fmt.Errorf(
			"reading pvc/%s to size the new claim: %w; pass --size to set it directly",
			claim.ExistingClaim, err)
	}

	size, err := claimSize(&source, override)
	if err != nil {
		return spec, err
	}
	spec.Resources = corev1.VolumeResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceStorage: size},
	}
	return spec, nil
}

func claimSize(source *corev1.PersistentVolumeClaim, override string) (resource.Quantity, error) {
	if override != "" {
		return resource.ParseQuantity(override)
	}
	if size, ok := source.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		return size, nil
	}
	return resource.Quantity{}, fmt.Errorf(
		"pvc/%s requests no storage size; pass --size", source.Name)
}

func restoreInPlace(
	ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup, o *restoreOptions,
) error {
	src, err := o.filesSource(sb)
	if err != nil {
		return err
	}
	var claims []string
	for _, p := range src.paths {
		v := backup.MountFor(sb, p)
		if v == nil {
			return fmt.Errorf("%w: %s is on none of scheduledbackup/%s's claims", ErrNoSourceVolume, p, sb.Name)
		}
		if !slices.Contains(claims, "pvc/"+v.ExistingClaim) {
			claims = append(claims, "pvc/"+v.ExistingClaim)
		}
	}

	if err := requireCurrentBackup(ctx, c, sb); err != nil {
		return err
	}
	what := fmt.Sprintf("%s under %s in %s in namespace %s",
		strings.Join(src.paths, ", "), src.tag, strings.Join(claims, ", "), sb.Namespace)
	if err := confirm(f, o.yes || o.dryRun, what, sb.Name); err != nil {
		return err
	}
	if job, err := activeBackup(ctx, c, sb); err != nil {
		return err
	} else if job != "" && !o.dryRun {
		return fmt.Errorf("%w: job/%s is backing up scheduledbackup/%s; wait for it with b2bk wait %s, "+
			"or stop it with b2bk cancel %s", ErrBackupRunning, job, sb.Name, sb.Name, sb.Name)
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}
	wasSuspended := sb.Spec.Suspend
	resume, err := suspendForRestore(ctx, c, sb, o.dryRun)
	if err != nil {
		return err
	}
	err = run.Attach(ctx, sb, runner.Options{Purpose: purposeRestore, MountData: true, Restore: true},
		defaultResticTimeout, func(pod *corev1.Pod) error {
			return o.restoreVolumes(ctx, runnerPod{run: run, pod: pod, stderr: f.Streams.ErrOut}, src,
				inPlaceTarget(sb), f.Streams.Out)
		})
	p := newPrinter(f.Streams.ErrOut)
	if err != nil {
		if !o.dryRun && !wasSuspended {
			p.printf("! scheduledbackup/%s stays suspended: the claims may hold a partial restore, which the\n", sb.Name)
			p.printf("! next backup would save and retention could then keep over good snapshots.\n")
			p.printf("! Check them, then: b2bk resume %s\n", sb.Name)
		}
		return errors.Join(err, p.Err())
	}
	if err := resume(); err != nil {
		p.printf("! could not restore spec.suspend on scheduledbackup/%s: %v\n", sb.Name, err)
		p.printf("! check it with: b2bk status %s\n", sb.Name)
	}
	return p.Err()
}

// activeBackup returns a backup Job of this ScheduledBackup that has not finished:
// one the CronJob started, or a triggered one.
func activeBackup(ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup) (string, error) {
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
		owner := metav1.GetControllerOf(job)
		if owner == nil || !slices.Contains(owners, owner.UID) || jobDone(job) {
			continue
		}
		return job.Name, nil
	}
	return "", nil
}

func jobDone(job *batchv1.Job) bool {
	if job.Status.CompletionTime != nil {
		return true
	}
	for _, cond := range job.Status.Conditions {
		if (cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed) && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func suspendForRestore(
	ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, dryRun bool,
) (func() error, error) {
	if dryRun || sb.Spec.Suspend {
		return func() error { return nil }, nil
	}

	patch := client.MergeFrom(sb.DeepCopy())
	sb.Spec.Suspend = true
	if err := c.Patch(ctx, sb, patch); err != nil {
		return nil, fmt.Errorf("suspending scheduledbackup/%s: %w", sb.Name, err)
	}

	return func() error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
		defer cancel()

		var latest b2backupv1.ScheduledBackup
		key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}
		if err := c.Get(ctx, key, &latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Spec.Suspend = false
		return c.Patch(ctx, &latest, patch)
	}, nil
}

func restoreToDatabase(
	ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup, o *restoreOptions,
) error {
	db := sb.Spec.Database
	if db == nil {
		return fmt.Errorf("%w: scheduledbackup/%s has no spec.database", ErrNoDatabase, sb.Name)
	}
	src, err := o.databaseSource(sb)
	if err != nil {
		return err
	}
	if err := requireCurrentBackup(ctx, c, sb); err != nil {
		return err
	}

	target := databaseTarget(db, src.database)
	if err := confirm(f, o.yes || o.dryRun, target, sb.Name); err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}
	return run.Attach(ctx, sb, runner.Options{Purpose: "restoredb", MountDatabase: true}, defaultResticTimeout,
		func(pod *corev1.Pod) error {
			return o.restoreDatabase(ctx, runnerPod{run: run, pod: pod, stderr: f.Streams.ErrOut}, sb, src, target,
				f.Streams.Out, f.Streams.ErrOut)
		})
}

func (o *restoreOptions) restoreDatabase(
	ctx context.Context, pod restorePod, sb *b2backupv1.ScheduledBackup, src restoreSource, target string,
	out, errOut io.Writer,
) error {
	snap, kinds, err := o.resolve(ctx, pod, src)
	if err != nil {
		return err
	}
	if kind := kinds[snap.Paths[0]]; kind != fileNode {
		return fmt.Errorf("%w: %s in snapshot %s is a %s, not a database dump",
			ErrWrongSnapshot, snap.Paths[0], snap.short(), orNone(kind))
	}
	p := newPrinter(errOut)
	p.printf("restoring %s from %s in snapshot %s\n", target, snap.Paths[0], snap.short())
	if err := p.Err(); err != nil {
		return err
	}
	return pod.exec(ctx, restoreDatabaseCommand(sb, snap, src.database, o.dryRun), out)
}

// databaseTarget names the database a restore overwrites, as restoredb will
// resolve it: the database given, or for mariadb the spec's, or the one the
// credentials Secret names.
func databaseTarget(db *b2backupv1.DatabaseSpec, database string) string {
	where := ""
	if db.Host != "" {
		where = " on " + db.Host
	}
	switch {
	case database != "":
		return fmt.Sprintf("the %s database %q%s", db.Engine, database, where)
	case db.Engine == b2backupv1.DatabaseEngineMariaDB && db.Name != "":
		return fmt.Sprintf("the %s database %q%s", db.Engine, db.Name, where)
	}
	return fmt.Sprintf("the %s database that secret/%s names in dbname%s", db.Engine, db.EffectiveSecretName(), where)
}
