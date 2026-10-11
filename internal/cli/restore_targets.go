package cli

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/kube"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	purposeRestore  = "restore"
	purposeDownload = "download"
)

func restoreToDir(
	ctx context.Context, f *Factory, sb *b2backupv1.ScheduledBackup, o *restoreOptions,
) error {
	if set := o.unsupportedForDir(); len(set) > 0 {
		return fmt.Errorf(
			"%s cannot be used with --to: a download runs restic dump, which takes only --path",
			strings.Join(set, " and "))
	}

	dest, err := filepath.Abs(o.toDir)
	if err != nil {
		return err
	}

	if o.dryRun {
		return listSnapshot(ctx, f, sb, o, dest)
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("downloading %s from snapshot %s into %s\n", o.dumpPath(), o.snapshot, dest)
	if err := p.Err(); err != nil {
		return err
	}

	return run.Attach(ctx, sb, runner.Options{Purpose: purposeDownload}, defaultResticTimeout,
		func(pod *corev1.Pod) error {
			n, err := untarFrom(dest, func(w io.Writer) error {
				return run.Exec(ctx, pod, kube.ExecOptions{
					Command: resticCommand(o.resticDumpArgs()...),
					Stdout:  w,
					Stderr:  f.Streams.ErrOut,
				})
			})
			if err != nil {
				return err
			}

			done := newPrinter(f.Streams.ErrOut)
			done.printf("restored %d files into %s\n", n, dest)
			return done.Err()
		})
}

func listSnapshot(
	ctx context.Context, f *Factory, sb *b2backupv1.ScheduledBackup, o *restoreOptions, dest string,
) error {
	run, err := f.Runner()
	if err != nil {
		return err
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("listing %s in snapshot %s; nothing is written to %s\n", o.dumpPath(), o.snapshot, dest)
	if err := p.Err(); err != nil {
		return err
	}

	return run.Run(ctx, sb, runner.Options{
		Purpose: purposeDownload,
		Command: resticCommand(o.resticListArgs()...),
	}, f.Streams.Out, defaultResticTimeout)
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
	if len(sb.Spec.Volumes) == 0 {
		return fmt.Errorf("%w: scheduledbackup/%s has no spec.volume", ErrNoSourceVolume, sb.Name)
	}

	spec, err := newClaimSpec(ctx, c, sb, o.size)
	if err != nil {
		return err
	}

	pvc := &corev1.PersistentVolumeClaim{
		Namespace: sb.Namespace,
		Name:      o.toNewPVC,
		Labels:    map[string]string{"app.kubernetes.io/managed-by": runner.ManagedByValue},
		Spec:      spec,
	}
	if err := c.Create(ctx, pvc); err != nil {
		return fmt.Errorf("creating pvc/%s: %w", o.toNewPVC, err)
	}

	size := spec.Resources.Requests[corev1.ResourceStorage]
	p := newPrinter(f.Streams.ErrOut)
	p.printf("created pvc/%s (%s)\n", pvc.Name, size.String())
	if err := p.Err(); err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}

	err = run.Run(ctx, sb, runner.Options{
		Purpose: purposeRestore,
		Command: resticCommand(o.resticRestoreArgs(restoreMountPath)...),
		ExtraVolumes: []corev1.Volume{{
			Name: "restore-target",
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc.Name,
			},
		}},
		ExtraMounts: []corev1.VolumeMount{{Name: "restore-target", MountPath: restoreMountPath}},
	}, f.Streams.Out, defaultResticTimeout)
	if err != nil {
		return err
	}

	done := newPrinter(f.Streams.ErrOut)
	done.printf("restored into pvc/%s; the claim is ready to mount\n", pvc.Name)
	return done.Err()
}

func newClaimSpec(
	ctx context.Context, c client.Client, sb *b2backupv1.ScheduledBackup, override string,
) (corev1.PersistentVolumeClaimSpec, error) {
	spec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}

	var source corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.Volumes[0].ExistingClaim}
	switch err := c.Get(ctx, key, &source); {
	case err == nil:
		if len(source.Spec.AccessModes) > 0 {
			spec.AccessModes = source.Spec.AccessModes
		}
		spec.StorageClassName = source.Spec.StorageClassName
	case override == "":
		return spec, fmt.Errorf(
			"reading pvc/%s to size the new claim: %w; pass --size to set it directly",
			sb.Spec.Volumes[0].ExistingClaim, err)
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
	if len(sb.Spec.Volumes) == 0 {
		return fmt.Errorf("%w: scheduledbackup/%s has no spec.volume", ErrNoSourceVolume, sb.Name)
	}

	claim := sb.Spec.Volumes[0].ExistingClaim
	if err := confirm(f, o.yes || o.dryRun, fmt.Sprintf("pvc/%s in namespace %s", claim, sb.Namespace), sb.Name); err != nil {
		return err
	}

	restore, err := suspendForRestore(ctx, c, sb, o.dryRun)
	if err != nil {
		return err
	}

	defer func() {
		if err := restore(); err != nil {
			p := newPrinter(f.Streams.ErrOut)
			p.printf("! could not restore spec.suspend on scheduledbackup/%s: %v\n", sb.Name, err)
			p.printf("! check it with: b2bk status %s\n", sb.Name)
		}
	}()

	run, err := f.Runner()
	if err != nil {
		return err
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("restoring snapshot %s over pvc/%s\n", o.snapshot, claim)
	if err := p.Err(); err != nil {
		return err
	}

	return run.Run(ctx, sb, runner.Options{
		Purpose:   purposeRestore,
		MountData: true,
		Command:   resticCommand(o.resticRestoreArgs(sb.Spec.Volumes[0].EffectiveMountPath())...),
	}, f.Streams.Out, defaultResticTimeout)
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
	ctx context.Context, f *Factory, sb *b2backupv1.ScheduledBackup, o *restoreOptions,
) error {
	db := sb.Spec.Database
	if db == nil {
		return fmt.Errorf("%w: scheduledbackup/%s has no spec.database", ErrNoDatabase, sb.Name)
	}

	target := fmt.Sprintf("the %s database %q on %s", db.Engine, db.Name, db.Host)
	if db.Host == "" {
		target = fmt.Sprintf("the %s database", db.Engine)
	}
	if err := confirm(f, o.yes || o.dryRun, target, sb.Name); err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("restoring %s from snapshot %s\n", target, o.snapshot)
	if err := p.Err(); err != nil {
		return err
	}

	return run.Run(ctx, sb, runner.Options{
		Purpose:       "restoredb",
		MountDatabase: true,
		Command:       restoreDatabaseCommand(sb, o),
	}, f.Streams.Out, defaultResticTimeout)
}

func restoreDatabaseCommand(sb *b2backupv1.ScheduledBackup, o *restoreOptions) []string {
	db := sb.Spec.Database
	dumpFile := sb.EffectiveResticHost() + dumpExtension(db.Engine)

	dump := "dump"
	if tag := databaseSourceTag(sb); tag != "" {
		dump += " --tag=" + shellQuote(tag)
	}

	pipeline := fmt.Sprintf("restic %s %s %s | restoredb %s --secret-mount=%s",
		dump, shellQuote(o.snapshot), shellQuote(dumpFile), string(db.Engine),
		b2backupv1.DBSecretMountPath)
	if o.dryRun {
		pipeline += " --dry-run"
	}

	guarded := fmt.Sprintf(
		"set -o pipefail\ncommand -v restoredb >/dev/null 2>&1 || { echo %s >&2; exit 1; }\n%s",
		shellQuote("this backup image has no restoredb; re-pull ghcr.io/usa-reddragon/b2-backup-operator-restic "+
			"or unpin spec.image"), pipeline)

	return []string{"sh", "-c", guarded}
}

func dumpExtension(engine b2backupv1.DatabaseEngine) string {
	switch engine {
	case b2backupv1.DatabaseEngineCNPG:
		return ".dmp"
	default:
		return ".sql"
	}
}

func databaseSourceTag(sb *b2backupv1.ScheduledBackup) string {
	for i := range sb.Spec.Sources {
		switch sb.Spec.Sources[i].Type {
		case b2backupv1.SourceTypeCNPG, b2backupv1.SourceTypeMariaDB:
			return sb.Spec.Sources[i].EffectiveTag()
		}
	}
	return ""
}
