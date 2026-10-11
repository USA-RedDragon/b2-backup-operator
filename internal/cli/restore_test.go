package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	latestSnapshot     = "latest"
	sourceStorageClass = "fast"
)

func withVolume(sb *b2backupv1.ScheduledBackup) {
	sb.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: testClaimName, MountPath: appMount}}
	sb.Spec.Sources = append(sb.Spec.Sources, b2backupv1.BackupSource{Type: b2backupv1.SourceTypeFiles})
}

func withDatabase(sb *b2backupv1.ScheduledBackup) {
	sb.Spec.Database = &b2backupv1.DatabaseSpec{
		Engine: b2backupv1.DatabaseEngineCNPG, Host: "postgresql-rw", Name: appDir, User: appDir,
	}
}

func TestRestoreRefusesTwoTargets(t *testing.T) {
	o := &restoreOptions{inPlace: true, toDatabase: true}
	if got := o.targets(); len(got) != 2 {
		t.Fatalf("targets = %v", got)
	}

	sb := newBackup(testBackupName, testRepoName)
	repo := newRepo(testNS)
	f := &Factory{Streams: testStreams()}
	err := runRestore(context.Background(), f, newClient(t, sb, repo), sb, o)
	if err == nil || !strings.Contains(err.Error(), "choose one restore target") {
		t.Fatalf("expected a single-target error, got %v", err)
	}
}

func TestRestoreWithoutATargetIsRefusedNonInteractively(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	withVolume(sb)
	withDatabase(sb)
	repo := newRepo(testNS)

	var errOut bytes.Buffer
	f := &Factory{Streams: testStreams()}
	f.Streams.ErrOut = &errOut

	err := runRestore(context.Background(), f, newClient(t, sb, repo), sb, &restoreOptions{})
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("expected ErrNoTarget, got %v", err)
	}

	for _, want := range []string{"--to DIR", "--to-new-pvc", "--in-place", "--to-database", testClaimName} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("expected %q in the target list:\n%s", want, errOut.String())
		}
	}
}

func TestAvailableTargetsMatchTheBackup(t *testing.T) {
	dbOnly := newBackup(testBackupName, testRepoName)
	withDatabase(dbOnly)
	got := strings.Join(availableTargets(dbOnly), " ")
	if strings.Contains(got, "--in-place") || strings.Contains(got, "--to-new-pvc") {
		t.Errorf("a database-only backup was offered volume targets: %s", got)
	}
	if !strings.Contains(got, "--to-database") {
		t.Errorf("a database backup should offer --to-database: %s", got)
	}

	filesOnly := newBackup(testBackupName, testRepoName)
	withVolume(filesOnly)
	got = strings.Join(availableTargets(filesOnly), " ")
	if strings.Contains(got, "--to-database") {
		t.Errorf("a files-only backup was offered --to-database: %s", got)
	}
}

func TestRestoreRejectsMissingSources(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	repo := newRepo(testNS)
	c := newClient(t, sb, repo)
	f := &Factory{Streams: testStreams()}
	ctx := context.Background()

	if err := runRestore(ctx, f, c, sb, &restoreOptions{inPlace: true, yes: true}); !errors.Is(err, ErrNoFilesSource) {
		t.Errorf("expected ErrNoFilesSource, got %v", err)
	}
	if err := runRestore(ctx, f, c, sb, &restoreOptions{toDatabase: true, yes: true}); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("expected ErrNoDatabase, got %v", err)
	}
}

func TestConfirmRequiresTheExactName(t *testing.T) {
	f := &Factory{Streams: testStreams()}
	f.Streams.In = strings.NewReader("wrong-name\n")
	f.stdinOnce.Do(func() { f.stdin = bufio.NewReader(f.Streams.In) })
	f.interactive = ptr.To(true)
	if err := confirm(f, false, "pvc/app-data", testBackupName); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("expected ErrNotConfirmed, got %v", err)
	}

	f = &Factory{Streams: testStreams()}
	f.Streams.In = strings.NewReader(testBackupName + "\n")
	f.stdinOnce.Do(func() { f.stdin = bufio.NewReader(f.Streams.In) })
	f.interactive = ptr.To(true)
	if err := confirm(f, false, "pvc/app-data", testBackupName); err != nil {
		t.Errorf("the exact name should confirm, got %v", err)
	}

	if err := confirm(&Factory{Streams: testStreams()}, true, "x", "y"); err != nil {
		t.Errorf("skip should bypass confirmation, got %v", err)
	}
}

func TestConfirmRefusesWithoutATerminal(t *testing.T) {
	f := &Factory{Streams: testStreams()}
	err := confirm(f, false, "pvc/app-data", testBackupName)
	if !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("expected ErrNotConfirmed, got %v", err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the error should name --yes: %v", err)
	}
}

func TestPromptsShareOneReader(t *testing.T) {
	f := &Factory{Streams: testStreams()}
	f.Streams.In = strings.NewReader("1\n" + testBackupName + "\n")

	first, err := f.Stdin().ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Stdin().ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(first) != "1" || strings.TrimSpace(second) != testBackupName {
		t.Errorf("reads = %q then %q; the second prompt lost its input",
			strings.TrimSpace(first), strings.TrimSpace(second))
	}
}

func offlineFactory(t *testing.T, streams genericiooptions.IOStreams) *Factory {
	t.Helper()
	f := &Factory{Streams: streams, ConfigFlags: genericclioptions.NewConfigFlags(true)}
	*f.ConfigFlags.Namespace = testNS
	f.restConfig = &rest.Config{Host: "http://127.0.0.1:1"}
	f.once.restConfig.Do(func() {})
	return f
}

func TestRestoreToDirRefusesFlagsItCannotHonour(t *testing.T) {
	tests := []struct {
		name string
		o    restoreOptions
		want []string
	}{
		{"include", restoreOptions{include: []string{"app/**"}}, []string{"--include"}},
		{"exclude", restoreOptions{exclude: []string{"**/cache"}}, []string{"--exclude"}},
		{"delete", restoreOptions{delete: true}, []string{"--delete"}},
		{
			name: "all three",
			o:    restoreOptions{include: []string{"a"}, exclude: []string{"b"}, delete: true},
			want: []string{"--include and --exclude and --delete"},
		},
	}

	sb := newBackup(testBackupName, testRepoName)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			o := tt.o
			o.toDir = dest
			o.snapshot = latestSnapshot

			err := restoreToDir(context.Background(), offlineFactory(t, testStreams()), sb, &o)
			if err == nil {
				t.Fatal("expected the unsupported flags to be refused")
			}
			for _, want := range append(tt.want, "cannot be used with --to") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("expected %q in %v", want, err)
				}
			}
			if _, err := os.Stat(dest); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the download directory was created anyway: %v", err)
			}
		})
	}
}

func sourceClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Namespace: testNS, Name: testClaimName,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To(sourceStorageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
		},
	}
}

func TestNewClaimSpecCopiesTheSourceClaim(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	withVolume(sb)
	c := newClient(t, sb, sourceClaim())
	ctx := context.Background()

	spec, err := newClaimSpec(ctx, c, sb, &sb.Spec.Volumes[0], "")
	if err != nil {
		t.Fatalf("newClaimSpec: %v", err)
	}
	size := spec.Resources.Requests[corev1.ResourceStorage]
	if size.String() != "20Gi" {
		t.Errorf("size = %s, want 20Gi", size.String())
	}
	if len(spec.AccessModes) != 1 || spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("accessModes = %v, want the source claim's", spec.AccessModes)
	}
	if spec.StorageClassName == nil || *spec.StorageClassName != sourceStorageClass {
		t.Errorf("storageClassName = %v, want the source claim's", spec.StorageClassName)
	}

	spec, err = newClaimSpec(ctx, c, sb, &sb.Spec.Volumes[0], "50Gi")
	size = spec.Resources.Requests[corev1.ResourceStorage]
	if err != nil || size.String() != "50Gi" {
		t.Errorf("--size override = %s, %v", size.String(), err)
	}
	if spec.StorageClassName == nil || *spec.StorageClassName != sourceStorageClass {
		t.Errorf("--size should not drop the source storage class: %v", spec.StorageClassName)
	}
}

func TestNewClaimSpecWithoutASourceClaim(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	withVolume(sb)
	c := newClient(t, sb)
	ctx := context.Background()

	if _, err := newClaimSpec(ctx, c, sb, &sb.Spec.Volumes[0], ""); err == nil || !strings.Contains(err.Error(), "--size") {
		t.Errorf("expected a hint to pass --size, got %v", err)
	}

	spec, err := newClaimSpec(ctx, c, sb, &sb.Spec.Volumes[0], "50Gi")
	if err != nil {
		t.Fatalf("--size should stand in for a missing source claim: %v", err)
	}
	if len(spec.AccessModes) != 1 || spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v, want the ReadWriteOnce default", spec.AccessModes)
	}
	if spec.StorageClassName != nil {
		t.Errorf("storageClassName = %v, want the cluster default", *spec.StorageClassName)
	}

	sized := sourceClaim()
	sized.Spec.Resources = corev1.VolumeResourceRequirements{}
	if _, err := newClaimSpec(ctx, newClient(t, sb, sized), sb, &sb.Spec.Volumes[0], ""); err == nil ||
		!strings.Contains(err.Error(), "--size") {
		t.Errorf("expected a hint to pass --size, got %v", err)
	}
}

func TestRestoreToNewPVCCreatesNoClaimBeforeTheSnapshotIsFound(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	withVolume(sb)
	c := newClient(t, sb, sourceClaim())
	ctx := context.Background()

	o := &restoreOptions{toNewPVC: "web-files-restore", snapshot: latestSnapshot}
	if err := restoreToNewPVC(ctx, offlineFactory(t, testStreams()), c, sb, o); err == nil {
		t.Fatal("expected the offline factory to fail")
	}
	var pvc corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: testNS, Name: o.toNewPVC}
	if err := c.Get(ctx, key, &pvc); !apierrors.IsNotFound(err) {
		t.Errorf("a claim was created before any snapshot was found to fill it: %v", err)
	}
}

func TestRestoreClaimCopiesTheSourceClaim(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	withVolume(sb)
	c := newClient(t, sb, sourceClaim())
	spec, err := newClaimSpec(context.Background(), c, sb, &sb.Spec.Volumes[0], "")
	if err != nil {
		t.Fatal(err)
	}
	pvc := newRestoreClaim(sb, "web-files-restore", spec)
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("accessModes = %v, want the source claim's", pvc.Spec.AccessModes)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != sourceStorageClass {
		t.Errorf("storageClassName = %v, want the source claim's", pvc.Spec.StorageClassName)
	}
	size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if size.String() != "20Gi" {
		t.Errorf("size = %s, want 20Gi", size.String())
	}
	if pvc.Labels["app.kubernetes.io/managed-by"] != runner.ManagedByValue {
		t.Errorf("labels = %v", pvc.Labels)
	}
	if len(pvc.OwnerReferences) != 0 {
		t.Errorf("the claim outlives the restore and should own nothing: %v", pvc.OwnerReferences)
	}
}

func TestSuspendForRestore(t *testing.T) {
	ctx := context.Background()
	suspended := func(t *testing.T, c client.Client, name string) bool {
		t.Helper()
		var got b2backupv1.ScheduledBackup
		if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: name}, &got); err != nil {
			t.Fatal(err)
		}
		return got.Spec.Suspend
	}

	t.Run("suspends and restores", func(t *testing.T) {
		sb := newBackup(testBackupName, testRepoName)
		c := newClient(t, sb)

		restore, err := suspendForRestore(ctx, c, sb, false)
		if err != nil {
			t.Fatal(err)
		}
		if !suspended(t, c, sb.Name) {
			t.Fatal("the schedule was not suspended for the restore")
		}
		if err := restore(); err != nil {
			t.Fatal(err)
		}
		if suspended(t, c, sb.Name) {
			t.Error("the schedule was left suspended")
		}
	})

	t.Run("dry run touches nothing", func(t *testing.T) {
		sb := newBackup(testBackupName, testRepoName)
		c := newClient(t, sb)

		restore, err := suspendForRestore(ctx, c, sb, true)
		if err != nil {
			t.Fatal(err)
		}
		if suspended(t, c, sb.Name) {
			t.Error("--dry-run suspended the schedule")
		}
		if err := restore(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an already suspended backup stays suspended", func(t *testing.T) {
		sb := newBackup(testBackupName, testRepoName)
		sb.Spec.Suspend = true
		c := newClient(t, sb)

		restore, err := suspendForRestore(ctx, c, sb, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := restore(); err != nil {
			t.Fatal(err)
		}
		if !suspended(t, c, sb.Name) {
			t.Error("a backup suspended before the restore was un-suspended by it")
		}
	})
}

func TestUntar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "app/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	write("app/config.php", "<?php")
	write("app/uploads/a.txt", "hello")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	n, err := untar(&buf, dest)
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 2 {
		t.Errorf("restored %d files, want 2", n)
	}
	body, err := os.ReadFile(filepath.Join(dest, "app/uploads/a.txt"))
	if err != nil || string(body) != "hello" {
		t.Errorf("file content = %q, %v", body, err)
	}
}

func TestUntarClampsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := "pwned"
	if err := tw.WriteHeader(&tar.Header{
		Name: "../../escape.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	n, err := untar(&buf, dest)
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored %d files, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a tar entry escaped the target directory: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "escape.txt"))
	if err != nil || string(got) != body {
		t.Errorf("the entry should land inside the target directory: %q, %v", got, err)
	}
}

func tarWith(t *testing.T, headers ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.WriteString(tw, strings.Repeat("x", int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func destAndOutside(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{dest, outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dest, outside
}

func TestUntarRefusesToWriteThroughASymlink(t *testing.T) {
	const escaped = "pwned"
	tests := []struct {
		name  string
		entry *tar.Header
	}{
		{
			name:  "regular file",
			entry: &tar.Header{Name: "evil/" + escaped, Mode: 0o644, Size: 5, Typeflag: tar.TypeReg},
		},
		{
			name:  "directory",
			entry: &tar.Header{Name: "evil/" + escaped + "/", Mode: 0o755, Typeflag: tar.TypeDir},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest, outside := destAndOutside(t)
			buf := tarWith(t,
				&tar.Header{Name: "evil", Linkname: outside, Mode: 0o777, Typeflag: tar.TypeSymlink},
				tt.entry,
			)

			if _, err := untar(buf, dest); err == nil {
				t.Fatal("untar followed a symlink out of the target directory")
			}
			if _, err := os.Lstat(filepath.Join(outside, escaped)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s was written outside the target directory: %v", escaped, err)
			}
		})
	}
}

func TestUntarKeepsSymlinksAndReplacesThem(t *testing.T) {
	dest, outside := destAndOutside(t)

	buf := tarWith(t,
		&tar.Header{Name: "app/", Mode: 0o755, Typeflag: tar.TypeDir},
		&tar.Header{Name: "app/link", Linkname: outside, Mode: 0o777, Typeflag: tar.TypeSymlink},
	)
	n, err := untar(buf, dest)
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 1 {
		t.Errorf("restored %d entries, want 1", n)
	}
	link, err := os.Readlink(filepath.Join(dest, "app/link"))
	if err != nil || link != outside {
		t.Fatalf("symlink = %q, %v; an escaping link should still be created", link, err)
	}

	buf = tarWith(t, &tar.Header{Name: "app/link", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg})
	if _, err := untar(buf, dest); err != nil {
		t.Fatalf("restoring over an existing symlink: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dest, "app/link"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("app/link = %v, %v; want a regular file", info, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "link")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the write followed the old symlink: %v", err)
	}
}

func TestUntarFromReportsTheUntarErrorWithoutDeadlocking(t *testing.T) {
	dest, outside := destAndOutside(t)
	buf := tarWith(t,
		&tar.Header{Name: "evil", Linkname: outside, Mode: 0o777, Typeflag: tar.TypeSymlink},
		&tar.Header{Name: "evil/pwned", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg},
	)

	got := make(chan error, 1)
	go func() {
		_, err := untarFrom(dest, func(w io.Writer) error {
			for _, chunk := range [][]byte{buf.Bytes(), make([]byte, 1<<20)} {
				if _, err := w.Write(chunk); err != nil {
					return fmt.Errorf("streaming from the pod: %w", err)
				}
			}
			return nil
		})
		got <- err
	}()

	select {
	case err := <-got:
		if err == nil {
			t.Fatal("untarFrom reported success for a symlink escape")
		}
		if !strings.Contains(err.Error(), "escapes from parent") {
			t.Fatalf("the untar error should be reported ahead of the producer's: %v", err)
		}
		if strings.Contains(err.Error(), "streaming from the pod") {
			t.Fatalf("the producer error hid the untar error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("untarFrom deadlocked: the producer was left blocked on the pipe")
	}
}

func TestDatabaseRestoreTargetsTheDatabaseSnapshot(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []b2backupv1.BackupSource{
		{Type: b2backupv1.SourceTypeCNPG},
		{Type: b2backupv1.SourceTypeFiles},
	}
	if got := databaseSourceTag(sb); got != "db" {
		t.Errorf("databaseSourceTag = %q, want %q", got, "db")
	}

	sb.Spec.Sources[0].Tag = testDBTag
	if got := databaseSourceTag(sb); got != testDBTag {
		t.Errorf("databaseSourceTag = %q, want %q", got, testDBTag)
	}

	filesOnly := newBackup(testBackupName, testRepoName)
	filesOnly.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}
	if got := databaseSourceTag(filesOnly); got != "" {
		t.Errorf("databaseSourceTag = %q, want empty", got)
	}
}

func TestChooseTargetMapsTheMenuToTheRightTarget(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		volume         bool
		database       bool
		wantInPlace    bool
		wantToDatabase bool
		wantErr        string
	}{
		{
			name:  "an empty answer takes the first, least destructive entry",
			input: "\n", volume: true, database: true,
			wantErr: "pass --to DIR",
		},
		{
			name:  "in place is third when both a volume and a database exist",
			input: "3\n", volume: true, database: true,
			wantInPlace: true,
		},
		{
			name:  "to database is fourth when both exist",
			input: "4\n", volume: true, database: true,
			wantToDatabase: true,
		},
		{
			name:  "to database is second when there is no volume",
			input: "2\n", database: true,
			wantToDatabase: true,
		},
		{
			name:  "in place is third even with no database",
			input: "3\n", volume: true,
			wantInPlace: true,
		},
		{
			name:  "a new claim still needs a name",
			input: "2\n", volume: true, database: true,
			wantErr: "pass --to-new-pvc NAME",
		},
		{
			name:  "a number past the end of the menu",
			input: "9\n", volume: true, database: true,
			wantErr: `"9" is not one of 1-4`,
		},
		{
			name:  "an answer that is not a number",
			input: "yes\n", volume: true, database: true,
			wantErr: `"yes" is not one of 1-4`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := newBackup(testBackupName, testRepoName)
			if tt.volume {
				withVolume(sb)
			}
			if tt.database {
				withDatabase(sb)
			}

			var errOut bytes.Buffer
			f := &Factory{Streams: testStreams()}
			f.Streams.ErrOut = &errOut
			f.Streams.In = strings.NewReader(tt.input)
			f.stdinOnce.Do(func() { f.stdin = bufio.NewReader(f.Streams.In) })
			f.interactive = ptr.To(true)

			var o restoreOptions
			err := chooseTarget(f, sb, &o)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("chooseTarget() = %+v, want an error", o)
				}
				if !errors.Is(err, ErrNoTarget) {
					t.Errorf("error = %v, want it to wrap ErrNoTarget", err)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("chooseTarget() error = %v", err)
			}

			if o.inPlace != tt.wantInPlace {
				t.Errorf("inPlace = %v, want %v; the menu picked the wrong thing to overwrite",
					o.inPlace, tt.wantInPlace)
			}
			if o.toDatabase != tt.wantToDatabase {
				t.Errorf("toDatabase = %v, want %v; the menu picked the wrong thing to overwrite",
					o.toDatabase, tt.wantToDatabase)
			}
			if !strings.Contains(errOut.String(), "1) --to DIR") {
				t.Errorf("the menu was not numbered from one:\n%s", errOut.String())
			}
		})
	}
}

func TestInPlaceRestoreWaitsForARunningBackupAndStaysSuspendedOnFailure(t *testing.T) {
	ctx := context.Background()
	suspended := func(t *testing.T, c client.Client) bool {
		t.Helper()
		var got b2backupv1.ScheduledBackup
		if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: testBackupName}, &got); err != nil {
			t.Fatal(err)
		}
		return got.Spec.Suspend
	}
	running := func(sb *b2backupv1.ScheduledBackup) []client.Object {
		cj := &batchv1.CronJob{Namespace: testNS, Name: backup.CronJobName(sb), UID: "uid-cj"}
		job := &batchv1.Job{Namespace: testNS, Name: testBackupName + "-backup-123", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "CronJob", Name: cj.Name, UID: cj.UID, Controller: ptr.To(true),
		}}}
		return []client.Object{cj, job}
	}

	t.Run("a backup in flight", func(t *testing.T) {
		sb := newBackup(testBackupName, testRepoName)
		withVolume(sb)
		c := newClient(t, append(running(sb), sb)...)
		err := restoreInPlace(ctx, offlineFactory(t, testStreams()), c, sb, &restoreOptions{snapshot: latest, yes: true})
		if !errors.Is(err, ErrBackupRunning) {
			t.Fatalf("err = %v, want ErrBackupRunning", err)
		}
		if suspended(t, c) {
			t.Error("a refused restore suspended the schedule")
		}
	})

	t.Run("a restore that fails", func(t *testing.T) {
		sb := newBackup(testBackupName, testRepoName)
		withVolume(sb)
		c := newClient(t, sb)
		var errOut bytes.Buffer
		streams := testStreams()
		streams.ErrOut = &errOut
		err := restoreInPlace(ctx, offlineFactory(t, streams), c, sb, &restoreOptions{snapshot: latest, yes: true})
		if err == nil {
			t.Fatal("expected the offline factory to fail")
		}
		if !suspended(t, c) {
			t.Error("the schedule was resumed over a claim that may hold a partial restore")
		}
		if !strings.Contains(errOut.String(), "b2bk resume") {
			t.Errorf("no hint to resume: %s", errOut.String())
		}
	})
}
