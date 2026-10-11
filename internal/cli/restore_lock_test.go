package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const ownRestoreJob = "web-files-b2bk-restore-abcde"

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
		{targetInPlace, filesAtData, "restic restore", func(ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup) error {
			return restoreInPlace(ctx, f, c, sb, &restoreOptions{snapshot: latest, yes: true})
		}},
		{"to the database", cnpgOnly, "restoredb", func(ctx context.Context, f *Factory, c client.Client, sb *b2backupv1.ScheduledBackup) error {
			return restoreToDatabase(ctx, f, c, sb, &restoreOptions{snapshot: latest, yes: true, toDatabase: true})
		}},
	}
}

// operatorSuspends stands in for the operator between the restore suspending
// the ScheduledBackup and its pod starting: it copies spec.suspend to the CronJob.
func operatorSuspends(t *testing.T, c client.Client, sb *b2backupv1.ScheduledBackup) {
	t.Helper()
	ctx := context.Background()
	var latest b2backupv1.ScheduledBackup
	if err := c.Get(ctx, client.ObjectKeyFromObject(sb), &latest); err != nil {
		t.Fatal(err)
	}
	var cj batchv1.CronJob
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: sb.Name + "-backup"}, &cj); err != nil {
		t.Fatal(err)
	}
	cj.Spec.Suspend = ptr.To(latest.Spec.Suspend)
	if err := c.Update(ctx, &cj); err != nil {
		t.Fatal(err)
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
			operatorSuspends(t, c, sb)
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
				APIVersion: "batch/v1", Kind: "CronJob", Name: fx.cj.Name, UID: fx.cj.UID, Controller: ptr.To(true),
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

func TestOverwritingRestoresSuspendAndResumeTheSchedule(t *testing.T) {
	t.Parallel()
	for _, target := range overwritingTargets() {
		t.Run(target.name, func(t *testing.T) {
			t.Parallel()
			t.Run("a restore that succeeds resumes it", func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				pod := &fakePod{}
				f, _ := hookedFactory(t, c, pod, true)
				if err := target.run(context.Background(), f, c, fx.sb); err != nil {
					t.Fatal(err)
				}
				got := backupState(t, c)
				if got.Spec.Suspend || len(got.Annotations) > 0 {
					t.Errorf("suspend = %t, annotations = %v; want the schedule back as it was", got.Spec.Suspend, got.Annotations)
				}
				if !strings.Contains(strings.Join(pod.commands, "\n"), target.failOn) {
					t.Errorf("nothing was restored: %v", pod.commands)
				}
			})
			t.Run("a restore that fails after writing stays suspended", func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				f, errOut := hookedFactory(t, c, &failingPod{failOn: target.failOn}, true)
				if err := target.run(context.Background(), f, c, fx.sb); !errors.Is(err, errRestoreFailed) {
					t.Fatalf("err = %v, want the restore's failure", err)
				}
				got := backupState(t, c)
				if !got.Spec.Suspend {
					t.Error("the schedule was resumed over a target that may hold a partial restore")
				}
				if _, held := got.Annotations[b2backupv1.AnnotationRestoreLock]; held {
					t.Error("a finished restore kept the lock")
				}
				if !strings.Contains(errOut.String(), "b2bk resume") {
					t.Errorf("no hint to resume: %s", errOut.String())
				}

				f, _ = hookedFactory(t, c, &fakePod{}, true)
				sb := got
				if err := target.run(context.Background(), f, c, &sb); err != nil {
					t.Fatal(err)
				}
				if got := backupState(t, c); got.Spec.Suspend {
					t.Error("a later restore that succeeded did not resume the schedule the failed one suspended")
				}
			})
			t.Run("a restore stopped before writing resumes it", func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				c := newClient(t, fx.objects()...)
				pod := &fakePod{}
				f, _ := hookedFactory(t, c, pod, false)
				if err := target.run(context.Background(), f, c, fx.sb); !errors.Is(err, ErrNotCurrent) {
					t.Fatalf("err = %v, want the restore stopped while the CronJob still runs", err)
				}
				if got := backupState(t, c); got.Spec.Suspend {
					t.Error("a restore that wrote nothing left the schedule suspended")
				}
				if strings.Contains(strings.Join(pod.commands, "\n"), target.failOn) {
					t.Errorf("restored while the CronJob could still fire: %v", pod.commands)
				}
			})
			t.Run("a schedule suspended beforehand stays suspended", func(t *testing.T) {
				t.Parallel()
				fx := currentBackup(t, target.mutate)
				fx.sb.Spec.Suspend = true
				c := newClient(t, fx.objects()...)
				f, _ := hookedFactory(t, c, &fakePod{}, true)
				if err := target.run(context.Background(), f, c, fx.sb); err != nil {
					t.Fatal(err)
				}
				if got := backupState(t, c); !got.Spec.Suspend {
					t.Error("a restore resumed a schedule someone else suspended")
				}
			})
		})
	}
}

func TestOnlyOneRestoreHoldsABackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sb := newBackup(testBackupName, testRepoName)
	c := newClient(t, sb)
	first, err := acquireRestoreLock(ctx, c, sb.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireRestoreLock(ctx, c, sb.DeepCopy()); !errors.Is(err, ErrRestoreRunning) {
		t.Fatalf("err = %v, want the second restore refused", err)
	}
	if got := backupState(t, c); !got.Spec.Suspend {
		t.Fatal("the schedule was not suspended")
	}
	if resumed, err := first.release(true); err != nil || !resumed {
		t.Fatalf("release = %t, %v", resumed, err)
	}
	if got := backupState(t, c); got.Spec.Suspend {
		t.Error("the schedule was left suspended")
	}
}

func TestSuspendAndResumeForgetARestoresSuspension(t *testing.T) {
	t.Parallel()
	for _, suspend := range []bool{true, false} {
		sb := newBackup(testBackupName, testRepoName)
		sb.Spec.Suspend = true
		sb.Annotations = map[string]string{b2backupv1.AnnotationRestoreResume: "true"}
		c := newClient(t, sb)
		if err := runSetSuspend(context.Background(), c, io.Discard, testNS, subjectBackup, suspend, "done"); err != nil {
			t.Fatal(err)
		}
		got := backupState(t, c)
		if _, ok := got.Annotations[b2backupv1.AnnotationRestoreResume]; ok || got.Spec.Suspend != suspend {
			t.Errorf("suspend %t: spec.suspend = %t, annotations = %v", suspend, got.Spec.Suspend, got.Annotations)
		}
	}
}
