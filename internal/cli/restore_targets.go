package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/kube"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	purposeRestore   = "restore"
	purposeRestoreDB = "restoredb"
	purposeDownload  = "download"
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
	if err := requireRepositoryOwnsItsSecret(ctx, f, sb); err != nil {
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

	return f.attachPod(ctx, runner.ForBackup(sb), runner.Options{Purpose: purposeDownload},
		func(pod restorePod, _ string) error {
			return o.download(ctx, pod, src, dest, f.Streams.Out, f.Streams.ErrOut)
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
	var dirs []dirMeta
	asRoot := os.Geteuid() == 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return count, finishDirs(root, dirs, asRoot)
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
			if err := root.MkdirAll(name, 0o755); err != nil {
				return count, err
			}
			dirs = append(dirs, dirMeta{name: name, header: header})
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
				return count, err
			}
			_ = root.Remove(name)
			if err := writeFile(root, name, 0o600, tr); err != nil {
				return count, err
			}
			if err := setMetadata(root, name, header, asRoot); err != nil {
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
			if asRoot {
				if err := root.Lchown(name, header.Uid, header.Gid); err != nil {
					return count, err
				}
			}
			count++
		}
	}
}

// dirMeta is a directory whose mode and times are set once everything inside
// it is written: writing a file changes its directory's mtime, and a read-only
// mode would stop the writes.
type dirMeta struct {
	name   string
	header *tar.Header
}

func finishDirs(root *os.Root, dirs []dirMeta, asRoot bool) error {
	for _, dir := range slices.Backward(dirs) {
		if err := setMetadata(root, dir.name, dir.header, asRoot); err != nil {
			return err
		}
	}
	return nil
}

// setMetadata gives a restored file or directory the owner, mode and times the
// archive recorded. The owner is set only when running as root, since nobody
// else may give a file away, and before the mode, since chown clears setuid
// and setgid.
func setMetadata(root *os.Root, name string, header *tar.Header, asRoot bool) error {
	if asRoot {
		if err := root.Lchown(name, header.Uid, header.Gid); err != nil {
			return err
		}
	}
	if err := root.Chmod(name, header.FileInfo().Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
		return err
	}
	if header.ModTime.IsZero() {
		return nil
	}
	atime := header.AccessTime
	if atime.IsZero() {
		atime = header.ModTime
	}
	return root.Chtimes(name, atime, header.ModTime)
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
	if err := requireRepositoryOwnsItsSecret(ctx, f, sb); err != nil {
		return err
	}
	claim, err := sourceVolume(sb, src)
	if err != nil {
		return err
	}
	if err := refuseExistingClaim(ctx, c, sb.Namespace, o.toNewPVC); err != nil {
		return err
	}

	spec, err := newClaimSpec(ctx, c, sb, claim, o.size)
	if err != nil {
		return err
	}

	size := spec.Resources.Requests[corev1.ResourceStorage]
	var plan volumePlan
	err = f.attachRestorePod(ctx, sb, runner.Options{Purpose: purposeRestore}, func(pod restorePod, _ string) error {
		plan, err = o.planVolumes(ctx, pod, src, newClaimTarget(sb, claim, restoreMountPath))
		if err != nil || !o.dryRun {
			return err
		}
		return o.runVolumes(ctx, pod, src, plan, f.Streams.Out)
	})
	if err != nil {
		return err
	}
	if o.dryRun {
		p := newPrinter(f.Streams.ErrOut)
		p.printf("dry run: would create pvc/%s (%s), laid out like pvc/%s, and restore into it mounted at %s; "+
			"nothing was created or restored\n", o.toNewPVC, size.String(), claim.ExistingClaim, restoreMountPath)
		return p.Err()
	}

	pvc := newRestoreClaim(sb, o.toNewPVC, spec)
	if err := c.Create(ctx, pvc); err != nil {
		return fmt.Errorf("creating pvc/%s: %w", o.toNewPVC, err)
	}

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
	wrote := false
	err = f.attachRestorePod(ctx, sb, opts, func(pod restorePod, _ string) error {
		wrote = true
		return o.runVolumes(ctx, pod, src, plan, f.Streams.Out)
	})
	if err != nil {
		releaseRestoreClaim(c, pvc, wrote, f.Streams.ErrOut)
		return err
	}

	done := newPrinter(f.Streams.ErrOut)
	done.printf("restored into pvc/%s; the claim is ready to mount\n", pvc.Name)
	return done.Err()
}

// refuseExistingClaim stops --to-new-pvc before it starts anything when the
// claim it would create is already there. A claim an earlier b2bk restore left
// may hold part of a restore, and restic restore into it would merge two
// snapshots' files, so it is never reused.
func refuseExistingClaim(ctx context.Context, c client.Client, namespace, name string) error {
	var existing corev1.PersistentVolumeClaim
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("checking for an existing pvc/%s: %w", name, err)
	case existing.Labels[labelManagedBy] == runner.ManagedByValue:
		return fmt.Errorf("%w: pvc/%s already exists; b2bk created it %s ago for an earlier restore, and it may "+
			"hold all or part of that restore. Mount it to check it, delete it with kubectl delete pvc -n %s %s, "+
			"or pass another name", ErrClaimExists, name, Age(existing.CreationTimestamp), namespace, name)
	default:
		return fmt.Errorf("%w: pvc/%s already exists and b2bk did not create it; pass another name",
			ErrClaimExists, name)
	}
}

// releaseRestoreClaim handles the claim --to-new-pvc created when the restore
// into it failed: an empty one is deleted, one restic may have written to is
// kept for the user to check.
func releaseRestoreClaim(c client.Client, pvc *corev1.PersistentVolumeClaim, wrote bool, errOut io.Writer) {
	p := newPrinter(errOut)
	if wrote {
		p.printf("! pvc/%s may hold a partial restore; check it, or delete it with: kubectl delete pvc -n %s %s\n",
			pvc.Name, pvc.Namespace, pvc.Name)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
	defer cancel()
	var opts []client.DeleteOption
	if pvc.UID != "" {
		opts = append(opts, client.Preconditions{UID: &pvc.UID})
	}
	err := c.Delete(ctx, pvc, opts...)
	switch {
	case err == nil, apierrors.IsNotFound(err):
		p.printf("deleted pvc/%s, which b2bk created for this restore; nothing was written to it\n", pvc.Name)
	default:
		p.printf("! could not delete pvc/%s, which b2bk created for this restore and wrote nothing to: %v\n",
			pvc.Name, err)
		p.printf("! delete it with: kubectl delete pvc -n %s %s\n", pvc.Namespace, pvc.Name)
	}
}

// newRestoreClaim is the claim --to-new-pvc restores into. It owns nothing and
// nothing owns it, so it outlives the restore.
func newRestoreClaim(sb *b2backupv1.ScheduledBackup, name string, spec corev1.PersistentVolumeClaimSpec) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Namespace: sb.Namespace,
		Name:      name,
		Labels:    map[string]string{labelManagedBy: runner.ManagedByValue},
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
	for _, p := range src.paths {
		if backup.MountFor(sb, p) == nil {
			return fmt.Errorf("%w: %s is on none of scheduledbackup/%s's claims", ErrNoSourceVolume, p, sb.Name)
		}
	}
	var claims []string
	for _, claim := range restoredClaims(sb, src.paths) {
		claims = append(claims, "pvc/"+claim)
	}

	if err := requireCurrentBackup(ctx, c, sb); err != nil {
		return err
	}
	what := fmt.Sprintf("%s under %s in %s in namespace %s",
		strings.Join(src.paths, ", "), src.tag, strings.Join(claims, ", "), sb.Namespace)
	if err := confirm(f, o.yes || o.dryRun, what, sb.Name); err != nil {
		return err
	}
	var hold *restoreHold
	if !o.dryRun {
		hold, err = holdForRestore(ctx, c, sb, restoredClaims(sb, src.paths), false,
			claimTargets(sb, src.paths), directClaimTargets(sb, src.paths))
		if err != nil {
			return err
		}
	}

	wrote := false
	opts := runner.Options{Purpose: purposeRestore, MountData: true, Restore: true}
	err = f.attachRestorePod(ctx, sb, opts, func(pod restorePod, job string) error {
		plan, err := o.planVolumes(ctx, pod, src, inPlaceTarget(sb))
		if err != nil {
			return err
		}
		if hold != nil {
			hold.full = o.delete && o.path == "" && len(o.include) == 0 && len(o.exclude) == 0 && len(src.exclude) == 0 &&
				slices.Equal(slices.Sorted(slices.Values(plan.snap.Paths)), src.paths)
			if err := hold.readyToWrite(ctx, job); err != nil {
				return err
			}
		}
		wrote = true
		return o.runVolumes(ctx, pod, src, plan, f.Streams.Out)
	})
	return hold.finish(f, err, wrote)
}

// attachRestorePod runs fn against a pod started for sb, with the name of its Job.
func (f *Factory) attachRestorePod(
	ctx context.Context, sb *b2backupv1.ScheduledBackup, opts runner.Options,
	fn func(pod restorePod, job string) error,
) error {
	return f.attachPod(ctx, runner.ForBackup(sb), opts, fn)
}

// attachPod runs fn against a pod started for s, with the name of its Job.
func (f *Factory) attachPod(
	ctx context.Context, s runner.Subject, opts runner.Options,
	fn func(pod restorePod, job string) error,
) error {
	if f.attachHook != nil {
		return f.attachHook(ctx, s, opts, fn)
	}
	run, err := f.Runner()
	if err != nil {
		return err
	}
	return run.Attach(ctx, s, opts, defaultResticTimeout, func(pod *corev1.Pod) error {
		return fn(runnerPod{run: run, pod: pod, stderr: f.Streams.ErrOut}, pod.Labels[batchv1.JobNameLabel])
	})
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
	target := databaseTarget(sb, src.database)
	var hold *restoreHold
	if !o.dryRun {
		key := []string{databaseKey(sb, src.database)}
		if hold, err = holdForRestore(ctx, c, sb, nil, true, key, key); err != nil {
			return err
		}
		hold.full = true
	}

	wrote := false
	opts := runner.Options{Purpose: purposeRestoreDB, MountDatabase: true}
	err = f.attachRestorePod(ctx, sb, opts, func(pod restorePod, job string) error {
		return o.restoreDatabase(ctx, pod, sb, src, target, f.Streams.Out, f.Streams.ErrOut,
			func(snap resticSnapshot) error {
				if err := confirm(f, o.yes || o.dryRun, dumpConfirmation(target, src, snap), sb.Name); err != nil {
					return err
				}
				if hold != nil {
					if err := hold.readyToWrite(ctx, job); err != nil {
						return err
					}
				}
				wrote = true
				return nil
			})
	})
	return hold.finish(f, err, wrote)
}

// dumpConfirmation names what a database restore overwrites and with which dump.
func dumpConfirmation(target string, src restoreSource, snap resticSnapshot) string {
	return fmt.Sprintf("%s with the dump %s tagged %s from snapshot %s taken %s on %s",
		target, snap.Paths[0], src.tag, snap.short(), snap.Time.Format(time.RFC3339), snap.Hostname)
}

func (o *restoreOptions) restoreDatabase(
	ctx context.Context, pod restorePod, sb *b2backupv1.ScheduledBackup, src restoreSource, target string,
	out, errOut io.Writer, confirmDump func(resticSnapshot) error,
) error {
	snap, kinds, err := o.resolve(ctx, pod, src)
	if err != nil {
		return err
	}
	if kind := kinds[snap.Paths[0]]; kind != fileNode {
		return fmt.Errorf("%w: %s in snapshot %s is a %s, not a database dump",
			ErrWrongSnapshot, snap.Paths[0], snap.short(), orNone(kind))
	}
	if confirmDump != nil {
		if err := confirmDump(snap); err != nil {
			return err
		}
	}
	p := newPrinter(errOut)
	p.printf("restoring %s from %s in snapshot %s\n", target, snap.Paths[0], snap.short())
	if err := p.Err(); err != nil {
		return err
	}
	return pod.exec(ctx, restoreDatabaseCommand(sb, snap, src.database, o.dryRun), out)
}

// databaseTarget names the database a restore overwrites, as restoredb will
// resolve it: the database given, else DB_DATABASE from the environment, which
// for mariadb the operator sets from the spec and for cnpg spec.env may set,
// else the one the credentials Secret names; and the host the same way.
func databaseTarget(sb *b2backupv1.ScheduledBackup, database string) string {
	db := sb.Spec.Database
	host := db.Host
	if db.Engine != b2backupv1.DatabaseEngineMariaDB {
		host = sb.Spec.Env["DB_HOST"]
	}
	where := " on " + host
	if host == "" {
		where = fmt.Sprintf(" on the host secret/%s names", db.EffectiveSecretName())
	}
	switch {
	case database != "":
	case db.Engine == b2backupv1.DatabaseEngineMariaDB && db.Name != "":
		database = db.Name
	case sb.Spec.Env["DB_DATABASE"] != "":
		database = sb.Spec.Env["DB_DATABASE"]
	default:
		return fmt.Sprintf("the %s database that secret/%s names in dbname%s", db.Engine, db.EffectiveSecretName(), where)
	}
	return fmt.Sprintf("the %s database %q%s", db.Engine, database, where)
}
