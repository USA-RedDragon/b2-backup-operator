package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	ownRestoreJob   = "web-files-b2bk-restore-abcde"
	resticRestore   = "restic restore"
	cronJobKind     = "CronJob"
	otherBackup     = "other"
	appDataPartial  = `["pvc/app-data:/"]`
	mediaMount      = "/data/media"
	mediaClaim      = "media"
	batchAPIVersion = "batch/v1"
)

var errRestoreFailed = errors.New("restore failed halfway")

func filesAtData(sb *b2backupv1.ScheduledBackup) {
	sb.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: testClaimName, MountPath: dataDir}}
	sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}
}

func cnpgOnly(sb *b2backupv1.ScheduledBackup) {
	withDatabase(sb)
	sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}}
}

type failingPod struct {
	fakePod
	failOn string
}

func (p *failingPod) exec(ctx context.Context, argv []string, stdout io.Writer) error {
	if line := strings.Join(argv, " "); strings.Contains(line, p.failOn) {
		p.commands = append(p.commands, line)
		return errRestoreFailed
	}
	return p.fakePod.exec(ctx, argv, stdout)
}

type restoreTarget struct {
	name   string
	mutate func(*b2backupv1.ScheduledBackup)
	failOn string
	run    func(context.Context, *Factory, client.Client, *b2backupv1.ScheduledBackup) error
}

func overwritingTargets() []restoreTarget {
	return []restoreTarget{
		{targetInPlace, filesAtData, resticRestore, func(ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup) error {
			return restoreInPlace(ctx, f, c, sb, &restoreOptions{snapshot: latest, yes: true})
		}},
		{"to the database", cnpgOnly, "restoredb", func(ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup) error {
			return restoreToDatabase(ctx, f, c, sb, &restoreOptions{snapshot: latest, yes: true, toDatabase: true})
		}},
	}
}

// operatorSuspends stands in for the operator between the restore taking the
// ScheduledBackups and its pod starting: it renders each CronJob's suspend.
func operatorSuspends(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	var list b2backupv1.ScheduledBackupList
	if err := c.List(ctx, &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		sb := &list.Items[i]
		var cj batchv1.CronJob
		if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: backup.CronJobName(sb)}, &cj); err != nil {
			continue
		}
		cj.Spec.Suspend = ptr.To(sb.Spec.Suspend || backup.RestoreHold(sb) != "")
		if err := c.Update(ctx, &cj); err != nil {
			t.Fatal(err)
		}
	}
}

func hookedFactory(t *testing.T, c client.Client, pod restorePod, simulateOperator bool) (*Factory, *bytes.Buffer) {
	t.Helper()
	f := factoryWithClient(t, c)
	var errOut bytes.Buffer
	f.Streams.ErrOut = &errOut
	f.attachHook = func(_ context.Context, sb *b2backupv1.ScheduledBackup, _ runner.Options,
		fn func(restorePod, string) error) error {
		if simulateOperator {
			operatorSuspends(t, c)
		}
		return fn(pod, ownRestoreJob)
	}
	return f, &errOut
}

func backupState(t *testing.T, c client.Client) b2backupv1.ScheduledBackup {
	t.Helper()
	var got b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: testBackupName}, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOverwritingRestoresRefuseWhileABackupOrRestoreRuns(t *testing.T) {
	t.Parallel()
	b2bkJob := func(sb *b2backupv1.ScheduledBackup, purpose string) *batchv1.Job {
		job := &batchv1.Job{
			Namespace: testNS, Name: "web-files-b2bk-" + purpose + "-zzzzz",
			Labels: map[string]string{labelManagedBy: runner.ManagedByValue},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: b2backupv1.GroupVersion.String(), Kind: kindScheduledBackup, Name: sb.Name, UID: sb.UID,
			}},
		}
		if purpose != "" {
			job.Labels[runner.LabelPurpose] = purpose
		}
		return job
	}
	others := []struct {
		name    string
		add     func(fx *currentFixture) client.Object
		wantErr error
	}{
		{"a scheduled backup", func(fx *currentFixture) client.Object {
			return &batchv1.Job{Namespace: testNS, Name: "web-files-backup-1", OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchAPIVersion, Kind: cronJobKind, Name: fx.cj.Name, UID: fx.cj.UID, Controller: ptr.To(true),
			}}}
		}, ErrBackupRunning},
		{"another b2bk restore", func(fx *currentFixture) client.Object { return b2bkJob(fx.sb, purposeRestore) }, ErrBackupRunning},
		{"another b2bk database restore", func(fx *currentFixture) client.Object { return b2bkJob(fx.sb, purposeRestoreDB) }, ErrBackupRunning},
		{"a b2bk Job from before purposes were recorded", func(fx *currentFixture) client.Object { return b2bkJob(fx.sb, "") }, ErrBackupRunning},
		{"another restore holding the lock", func(fx *currentFixture) client.Object {
			fx.sb.Annotations = map[string]string{b2backupv1.AnnotationRestoreLock: "b2bk pid 1 on laptop"}
			fx.sb.Spec.Suspend = true
			return nil
		}, ErrRestoreRunning},
	}
	for _, target := range overwritingTargets() {
		for _, other := range others {
			t.Run(target.name+"/"+other.name, func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				var objs []client.Object
				if obj := other.add(&fx); obj != nil {
					objs = append(objs, obj)
				}
				wasSuspended := fx.sb.Spec.Suspend
				c := newClient(t, fx.objects(objs...)...)
				pod := &fakePod{}
				f, _ := hookedFactory(t, c, pod, true)
				err := target.run(context.Background(), f, c, fx.sb)
				if !errors.Is(err, other.wantErr) {
					t.Fatalf("err = %v, want %v", err, other.wantErr)
				}
				if len(pod.commands) > 0 {
					t.Errorf("a refused restore ran %v", pod.commands)
				}
				if got := backupState(t, c); got.Spec.Suspend != wasSuspended {
					t.Errorf("a refused restore changed spec.suspend to %t", got.Spec.Suspend)
				}
			})
		}
	}
}

func TestADownloadDoesNotHoldOffAnOverwritingRestore(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t, cnpgOnly)
	download := &batchv1.Job{
		Namespace: testNS, Name: "web-files-b2bk-download-zzzzz",
		Labels: map[string]string{labelManagedBy: runner.ManagedByValue, runner.LabelPurpose: purposeDownload},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: b2backupv1.GroupVersion.String(), Kind: kindScheduledBackup, Name: fx.sb.Name, UID: fx.sb.UID,
		}},
	}
	c := newClient(t, fx.objects(download)...)
	f, _ := hookedFactory(t, c, &fakePod{}, true)
	if err := restoreToDatabase(context.Background(), f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true, toDatabase: true}); err != nil {
		t.Fatal(err)
	}
}

func held(t *testing.T, c client.Client, name string) (string, []string) {
	t.Helper()
	var got b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Suspend {
		t.Errorf("scheduledbackup/%s: a restore wrote spec.suspend, which is the user's", name)
	}
	return got.Annotations[b2backupv1.AnnotationRestoreLock], partialTargets(&got)
}

func TestOverwritingRestoresHoldTheScheduleUntilTheTargetIsWhole(t *testing.T) {
	t.Parallel()
	for _, target := range overwritingTargets() {
		t.Run(target.name, func(t *testing.T) {
			t.Parallel()
			t.Run("a restore that succeeds lets go", func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				pod := &fakePod{}
				f, _ := hookedFactory(t, c, pod, true)
				if err := target.run(context.Background(), f, c, fx.sb); err != nil {
					t.Fatal(err)
				}
				if lock, partial := held(t, c, testBackupName); lock != "" || len(partial) > 0 {
					t.Errorf("lock = %q, partial = %v; want the schedule back as it was", lock, partial)
				}
				if !strings.Contains(strings.Join(pod.commands, "\n"), target.failOn) {
					t.Errorf("nothing was restored: %v", pod.commands)
				}
			})
			t.Run("a restore that fails after writing keeps it held", func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				f, errOut := hookedFactory(t, c, &failingPod{failOn: target.failOn}, true)
				if err := target.run(context.Background(), f, c, fx.sb); !errors.Is(err, errRestoreFailed) {
					t.Fatalf("err = %v, want the restore's failure", err)
				}
				lock, partial := held(t, c, testBackupName)
				if lock != "" || len(partial) != 1 {
					t.Fatalf("lock = %q, partial = %v; want the lock gone and the target recorded", lock, partial)
				}
				if !strings.Contains(errOut.String(), "b2bk resume") {
					t.Errorf("no hint to resume: %s", errOut.String())
				}
				current := backupState(t, c)
				if backup.RestoreHold(&current) == "" {
					t.Error("the operator would resume the CronJob over a partial restore")
				}

				f, _ = hookedFactory(t, c, &failingPod{failOn: "snapshots"}, true)
				if err := target.run(context.Background(), f, c, &current); err == nil {
					t.Fatal("the restore that cannot list snapshots succeeded")
				}
				if _, partial := held(t, c, testBackupName); len(partial) != 1 {
					t.Errorf("partial = %v; a restore that wrote nothing let go of a partial target", partial)
				}

				current = backupState(t, c)
				f, _ = hookedFactory(t, c, &fakePod{}, true)
				if err := target.run(context.Background(), f, c, &current); err != nil {
					t.Fatal(err)
				}
				if _, partial := held(t, c, testBackupName); len(partial) > 0 {
					t.Errorf("partial = %v after a complete restore of the same target", partial)
				}
			})
			t.Run("a restore stopped before writing records nothing", func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				pod := &fakePod{}
				f, _ := hookedFactory(t, c, pod, false)
				if err := target.run(context.Background(), f, c, fx.sb); !errors.Is(err, ErrNotCurrent) {
					t.Fatalf("err = %v, want the restore stopped while the CronJob still runs", err)
				}
				if lock, partial := held(t, c, testBackupName); lock != "" || len(partial) > 0 {
					t.Errorf("lock = %q, partial = %v after a restore that wrote nothing", lock, partial)
				}
				if strings.Contains(strings.Join(pod.commands, "\n"), target.failOn) {
					t.Errorf("restored while the CronJob could still fire: %v", pod.commands)
				}
			})
			t.Run("a spec changed after planning stops it before writing", func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				pod := &fakePod{}
				f := factoryWithClient(t, c)
				f.attachHook = func(ctx context.Context, sb *b2backupv1.ScheduledBackup, _ runner.Options,
					fn func(restorePod, string) error) error {
					current := backupState(t, c)
					current.Spec.Schedule = "@daily"
					if err := c.Update(ctx, &current); err != nil {
						t.Fatal(err)
					}
					operatorSuspends(t, c)
					return fn(pod, ownRestoreJob)
				}
				if err := target.run(context.Background(), f, c, fx.sb); !errors.Is(err, ErrNotCurrent) {
					t.Fatalf("err = %v, want the restore stopped", err)
				}
				if strings.Contains(strings.Join(pod.commands, "\n"), target.failOn) {
					t.Errorf("restored from a stale plan: %v", pod.commands)
				}
			})
		})
	}
}

func TestAPartialRestoreOfOneTargetIsNotClearedByAnother(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := currentBackup(t, func(sb *b2backupv1.ScheduledBackup) {
		filesAtData(sb)
		withDatabase(sb)
		sb.Spec.Sources = append(sb.Spec.Sources, b2backupv1.BackupSource{Type: b2backupv1.SourceTypeCNPG})
	})
	c := newClient(t, fx.objects()...)
	f, _ := hookedFactory(t, c, &failingPod{failOn: resticRestore}, true)
	if err := restoreInPlace(ctx, f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true}); !errors.Is(err, errRestoreFailed) {
		t.Fatalf("err = %v", err)
	}
	current := backupState(t, c)
	f, _ = hookedFactory(t, c, &fakePod{}, true)
	if err := restoreToDatabase(ctx, f, c, &current, &restoreOptions{snapshot: latest, yes: true, toDatabase: true}); err != nil {
		t.Fatal(err)
	}
	if _, partial := held(t, c, testBackupName); len(partial) != 1 || !strings.HasPrefix(partial[0], "pvc/") {
		t.Errorf("partial = %v; a database restore let go of a partial files restore", partial)
	}
}

func TestARestoreHoldsEveryBackupSharingItsClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := currentBackup(t, filesAtData)
	other := newBackup(otherBackup, testRepoName)
	other.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: testClaimName, MountPath: "/mnt"}}
	other.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles, Tag: "other"}}
	otherCronJob := &batchv1.CronJob{Namespace: testNS, Name: "other-backup", UID: "uid-other-cj"}
	running := &batchv1.Job{Namespace: testNS, Name: "other-backup-1", OwnerReferences: []metav1.OwnerReference{{
		APIVersion: batchAPIVersion, Kind: cronJobKind, Name: otherCronJob.Name, UID: otherCronJob.UID, Controller: ptr.To(true),
	}}}

	c := newClient(t, fx.objects(other, otherCronJob, running)...)
	f, _ := hookedFactory(t, c, &fakePod{}, true)
	if err := restoreInPlace(ctx, f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true}); !errors.Is(err, ErrBackupRunning) {
		t.Fatalf("err = %v, want the restore refused while the other backup of the claim runs", err)
	}

	c = newClient(t, fx.objects(other, otherCronJob)...)
	f, _ = hookedFactory(t, c, &failingPod{failOn: resticRestore}, true)
	if err := restoreInPlace(ctx, f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true}); !errors.Is(err, errRestoreFailed) {
		t.Fatalf("err = %v", err)
	}
	if _, partial := held(t, c, otherBackup); len(partial) != 1 {
		t.Errorf("partial = %v; the other backup of the claim would save the partial restore", partial)
	}
}

func TestNothingStartsABackupWhileARestoreHoldsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, annotation := range []string{b2backupv1.AnnotationRestoreLock, b2backupv1.AnnotationRestorePartial} {
		sb := newBackup(testBackupName, testRepoName)
		sb.Annotations = map[string]string{annotation: appDataPartial}
		c := newClient(t, sb)
		if err := triggerBackup(ctx, c, sb.DeepCopy(), time.Now()); !errors.Is(err, ErrRestoreRunning) {
			t.Errorf("%s: trigger err = %v, want it refused", annotation, err)
		}
	}

	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Suspend = true
	sb.Annotations = map[string]string{b2backupv1.AnnotationRestoreLock: "b2bk pid 1"}
	c := newClient(t, sb)
	if err := runSetSuspend(ctx, c, io.Discard, testNS, subjectBackup, false, "resumed"); !errors.Is(err, ErrRestoreRunning) {
		t.Errorf("resume err = %v, want it refused while a restore runs", err)
	}
}

func TestResumeLetsGoOfAPartialRestore(t *testing.T) {
	t.Parallel()
	sb := newBackup(testBackupName, testRepoName)
	sb.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: appDataPartial}
	c := newClient(t, sb)
	var out bytes.Buffer
	if err := runSetSuspend(context.Background(), c, &out, testNS, subjectBackup, false, "resumed"); err != nil {
		t.Fatal(err)
	}
	if _, partial := held(t, c, testBackupName); len(partial) > 0 {
		t.Errorf("partial = %v after b2bk resume", partial)
	}
	if !strings.Contains(out.String(), "pvc/app-data:/") {
		t.Errorf("resume did not say what it let go of: %s", out.String())
	}
}

func TestOnlyARestoreOfTheWholeSourceClearsAPartialOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := currentBackup(t, filesAtData)
	c := newClient(t, fx.objects()...)
	f, _ := hookedFactory(t, c, &failingPod{failOn: resticRestore}, true)
	if err := restoreInPlace(ctx, f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true}); !errors.Is(err, errRestoreFailed) {
		t.Fatalf("err = %v", err)
	}
	current := backupState(t, c)
	pod := &fakePod{}
	f, _ = hookedFactory(t, c, pod, true)
	if err := restoreInPlace(ctx, f, c, &current, &restoreOptions{snapshot: snapSub, yes: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(pod.commands, "\n"), snapSub+":/data/app") {
		t.Fatalf("the narrower snapshot was not restored: %v", pod.commands)
	}
	if _, partial := held(t, c, testBackupName); len(partial) != 1 {
		t.Errorf("partial = %v; a snapshot of /data/app alone let go of a partial restore of /data", partial)
	}
}

func TestARestoreHoldsAndMarksClaimsMountedUnderIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nested := func(sb *b2backupv1.ScheduledBackup) {
		filesAtData(sb)
		sb.Spec.Volumes = append(sb.Spec.Volumes, b2backupv1.VolumeSpec{ExistingClaim: mediaClaim, MountPath: mediaMount})
	}
	fx := currentBackup(t, nested)
	if got := claimTargets(fx.sb, []string{dataDir}); !slices.Contains(got, "pvc/media:/") {
		t.Errorf("targets = %v, want the claim mounted under /data", got)
	}
	other := newBackup(otherBackup, testRepoName)
	other.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: mediaClaim, MountPath: "/media"}}
	other.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles, Tag: otherBackup}}
	c := newClient(t, fx.objects(other)...)
	f, _ := hookedFactory(t, c, &failingPod{failOn: resticRestore}, true)
	if err := restoreInPlace(ctx, f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true}); !errors.Is(err, errRestoreFailed) {
		t.Fatalf("err = %v", err)
	}
	if _, partial := held(t, c, otherBackup); !slices.Contains(partial, "pvc/media:/") {
		t.Errorf("partial = %v; the backup of the nested claim would save a partial restore", partial)
	}
}

func TestARestoreStopsWhenAnotherBackupComesToShareItsTarget(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t, filesAtData)
	c := newClient(t, fx.objects()...)
	pod := &fakePod{}
	f := factoryWithClient(t, c)
	f.attachHook = func(ctx context.Context, _ *b2backupv1.ScheduledBackup, _ runner.Options,
		fn func(restorePod, string) error) error {
		late := newBackup(otherBackup, testRepoName)
		late.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: testClaimName, MountPath: "/mnt"}}
		if err := c.Create(ctx, late); err != nil {
			t.Fatal(err)
		}
		operatorSuspends(t, c)
		return fn(pod, ownRestoreJob)
	}
	err := restoreInPlace(context.Background(), f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true})
	if !errors.Is(err, ErrNotCurrent) || !strings.Contains(err.Error(), otherBackup) {
		t.Fatalf("err = %v, want the restore stopped for the new backup of the claim", err)
	}
	if strings.Contains(strings.Join(pod.commands, "\n"), resticRestore) {
		t.Errorf("restored beside a backup that is not held: %v", pod.commands)
	}
}

func TestDatabasesOnOtherHostsAreOtherTargets(t *testing.T) {
	t.Parallel()
	mariadbOn := func(name, host string) *b2backupv1.ScheduledBackup {
		sb := newBackup(name, testRepoName)
		sb.Spec.Database = &b2backupv1.DatabaseSpec{
			Engine: b2backupv1.DatabaseEngineMariaDB, Host: host, Name: appDir, User: appDir,
		}
		return sb
	}
	a, b := mariadbOn(testBackupName, "db-a"), mariadbOn(otherBackup, "db-b")
	if databaseKey(a, "") == databaseKey(b, "") {
		t.Errorf("one key %q for the same database name on two hosts", databaseKey(a, ""))
	}
	got, err := sharingBackups(context.Background(), newClient(t, a, b), a, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("held %d backups; a database on another host is not this one", len(got))
	}
}

func TestResumeLetsGoOfThePartialRestoreEverywhere(t *testing.T) {
	t.Parallel()
	entry := appDataPartial
	sb := newBackup(testBackupName, testRepoName)
	sb.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: entry}
	other := newBackup(otherBackup, testRepoName)
	other.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: entry}
	c := newClient(t, sb, other)
	if err := runSetSuspend(context.Background(), c, io.Discard, testNS, subjectBackup, false, "resumed"); err != nil {
		t.Fatal(err)
	}
	if _, partial := held(t, c, otherBackup); len(partial) > 0 {
		t.Errorf("partial = %v; the other backup of the claim stayed held after the data was checked", partial)
	}
}

func TestARestoreOfAParentNeverClearsANestedClaimItMayNotHaveWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := currentBackup(t, func(sb *b2backupv1.ScheduledBackup) {
		filesAtData(sb)
		sb.Spec.Volumes = append(sb.Spec.Volumes, b2backupv1.VolumeSpec{ExistingClaim: mediaClaim, MountPath: mediaMount})
		sb.Spec.Sources[0].Exclude = []string{mediaMount}
	})
	fx.sb.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: `["pvc/media:/"]`}
	c := newClient(t, fx.objects()...)
	f, _ := hookedFactory(t, c, &fakePod{}, true)
	if err := restoreInPlace(ctx, f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, partial := held(t, c, testBackupName); !slices.Contains(partial, "pvc/media:/") {
		t.Errorf("partial = %v; a restore of /data that excludes /data/media let go of media", partial)
	}
}

func TestADatabaseNamedByTheSecretIsSharedWithOneNamingItsHost(t *testing.T) {
	t.Parallel()
	a := newBackup(testBackupName, testRepoName)
	withDatabase(a)
	b := newBackup(otherBackup, testRepoName)
	withDatabase(b)
	b.Spec.Env = map[string]string{envDBHost: "postgresql-rw"}
	got, err := sharingBackups(context.Background(), newClient(t, a, b), a, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("held %d backups; the Secret's host may be the one the other names", len(got))
	}
}

func TestARestoreOfASourceWithExcludesNeverClearsAPartialRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := currentBackup(t, func(sb *b2backupv1.ScheduledBackup) {
		filesAtData(sb)
		sb.Spec.Sources[0].Exclude = []string{cacheDir}
	})
	fx.sb.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: appDataPartial}
	c := newClient(t, fx.objects()...)
	f, _ := hookedFactory(t, c, &fakePod{}, true)
	if err := restoreInPlace(ctx, f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, partial := held(t, c, testBackupName); len(partial) != 1 {
		t.Errorf("partial = %v; a restore that skips %s let go of a partial restore that may have written there",
			partial, cacheDir)
	}
}

func TestARestoreThroughAShadowingLayoutNeverClearsAnotherBackupsPartialRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := currentBackup(t, func(sb *b2backupv1.ScheduledBackup) {
		filesAtData(sb)
		sb.Spec.Volumes = append(sb.Spec.Volumes, b2backupv1.VolumeSpec{ExistingClaim: mediaClaim, MountPath: mediaMount})
	})
	fx.sb.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: appDataPartial}
	plain := newBackup(otherBackup, testRepoName)
	plain.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: testClaimName, MountPath: dataDir}}
	plain.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: appDataPartial}
	c := newClient(t, fx.objects(plain)...)
	f, _ := hookedFactory(t, c, &fakePod{}, true)
	if err := restoreInPlace(ctx, f, c, fx.sb, &restoreOptions{snapshot: latest, yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, partial := held(t, c, otherBackup); !slices.Contains(partial, "pvc/app-data:/") {
		t.Errorf("partial = %v; a restore with media mounted over /data/media let go of app-data, whose "+
			"/media it never wrote", partial)
	}
}

type recordingPod struct {
	fakePod
	t       *testing.T
	c       client.Client
	seen    []string
	writeOn string
}

func (p *recordingPod) exec(ctx context.Context, argv []string, stdout io.Writer) error {
	if strings.Contains(strings.Join(argv, " "), p.writeOn) {
		var sb b2backupv1.ScheduledBackup
		if err := p.c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: testBackupName}, &sb); err != nil {
			p.t.Fatal(err)
		}
		p.seen = partialTargets(&sb)
	}
	return p.fakePod.exec(ctx, argv, stdout)
}

func TestARestoreRecordsWhatItWritesBeforeItWrites(t *testing.T) {
	t.Parallel()
	for _, target := range overwritingTargets() {
		for _, narrow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s narrowed %t", target.name, narrow), func(t *testing.T) {
				t.Parallel()
				if narrow && target.name != targetInPlace {
					t.Skip("a database restore cannot be narrowed")
				}
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				pod := &recordingPod{t: t, c: c, writeOn: target.failOn}
				f, _ := hookedFactory(t, c, pod, true)
				var err error
				if narrow {
					err = restoreInPlace(context.Background(), f, c, fx.sb,
						&restoreOptions{snapshot: latest, yes: true, path: appDir})
				} else {
					err = target.run(context.Background(), f, c, fx.sb)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(pod.seen) == 0 {
					t.Error("the restore wrote before it recorded its target, so dying mid-write would leave nothing held")
				}
				if _, partial := held(t, c, testBackupName); len(partial) > 0 {
					t.Errorf("partial = %v after a restore that succeeded", partial)
				}
			})
		}
	}
}

func TestRestoresFromARepositoryThatIsNotItsOwnAreRefused(t *testing.T) {
	t.Parallel()
	fx := currentBackup(t, filesAtData)
	fx.repo.Status.Conditions = []metav1.Condition{{
		Type: b2backupv1.RepositoryConditionReady, Status: metav1.ConditionFalse, Reason: reasonRepositoryConflict,
	}}
	c := newClient(t, fx.objects()...)
	f := factoryWithClient(t, c)
	if err := restoreToDir(context.Background(), f, fx.sb, &restoreOptions{snapshot: latest, toDir: t.TempDir()}); !errors.Is(err, ErrRepositoryNotUsable) {
		t.Errorf("--to: err = %v", err)
	}
	if err := restoreToNewPVC(context.Background(), f, c, fx.sb, &restoreOptions{snapshot: latest, toNewPVC: "copy", size: "1Gi"}); !errors.Is(err, ErrRepositoryNotUsable) {
		t.Errorf("--to-new-pvc: err = %v", err)
	}
}

func TestResumeLeavesARunningRestoresEntriesAlone(t *testing.T) {
	t.Parallel()
	sb := newBackup(testBackupName, testRepoName)
	sb.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: appDataPartial}
	other := newBackup(otherBackup, testRepoName)
	other.Annotations = map[string]string{
		b2backupv1.AnnotationRestorePartial: appDataPartial,
		b2backupv1.AnnotationRestoreLock:    "b2bk pid 1",
	}
	c := newClient(t, sb, other)
	if err := runSetSuspend(context.Background(), c, io.Discard, testNS, subjectBackup, false, "resumed"); err != nil {
		t.Fatal(err)
	}
	var got b2backupv1.ScheduledBackup
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: otherBackup}, &got); err != nil {
		t.Fatal(err)
	}
	if len(partialTargets(&got)) != 1 {
		t.Error("b2bk resume removed what a running restore recorded before writing")
	}
}

func TestAnotherBackupsRestoreHoldShowsAndStopsATrigger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: testClaimName, MountPath: dataDir}}
	other := newBackup(otherBackup, testRepoName)
	other.Annotations = map[string]string{b2backupv1.AnnotationRestorePartial: appDataPartial}
	c := newClient(t, sb, other)
	if err := triggerBackup(ctx, c, sb.DeepCopy(), time.Now()); !errors.Is(err, ErrRestoreRunning) {
		t.Errorf("trigger err = %v, want it refused while another backup's restore left the claim partial", err)
	}
	var out bytes.Buffer
	if err := runStatus(ctx, c, &out, testNS, subjectBackup, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "scheduledbackup/"+otherBackup) {
		t.Errorf("status does not show the hold another backup's restore put on it:\n%s", out.String())
	}
}
