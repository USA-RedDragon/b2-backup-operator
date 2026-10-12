package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	testZone    = "America/Chicago"
	testContext = "admin@home"
	nameRestic  = "restic"
	nsGitea     = "gitea"
	nsVault     = "vaultwarden"
	nsMinecraft = "minecraft"
	nsKosync    = "kosync"
	tagFiles    = "files"
	tagDB       = "db"
	snapLatest  = "5d1e09ab"
	reasonReady = "Ready"
	claimData   = "data"
)

// testNow is the fixed clock every frame is drawn at.
var testNow = time.Date(2026, 10, 11, 23, 50, 16, 0, time.UTC)

func at(d time.Duration) *metav1.Time { t := metav1.NewTime(testNow.Add(d)); return &t }

func ready(gen int64) []metav1.Condition {
	return []metav1.Condition{{Type: reasonReady, Status: metav1.ConditionTrue, Reason: reasonReady, ObservedGeneration: gen,
		Message: "Backing up on schedule"}}
}

func scheduled(ns, schedule string, sources ...b2backupv1.BackupSource) *b2backupv1.ScheduledBackup {
	return &b2backupv1.ScheduledBackup{
		Namespace: ns, Name: nameRestic, UID: types.UID(ns + "-sb"), Generation: 1,
		Spec: b2backupv1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: nameRestic}, Schedule: "@daily", TimeZone: testZone,
			Sources: sources, Volumes: []b2backupv1.VolumeSpec{{ExistingClaim: claimData, MountPath: "/data"}},
			Retention: &b2backupv1.Retention{Daily: ptr.To(int32(90)), Weekly: ptr.To(int32(24)),
				Monthly: ptr.To(int32(24)), Yearly: ptr.To(int32(20))},
		},
		Status: b2backupv1.ScheduledBackupStatus{
			Conditions: ready(1), ObservedGeneration: 1, EffectiveSchedule: schedule,
		},
	}
}

func files(paths ...string) b2backupv1.BackupSource {
	return b2backupv1.BackupSource{Type: b2backupv1.SourceTypeFiles, Paths: paths}
}

func dbSource() b2backupv1.BackupSource {
	return b2backupv1.BackupSource{Type: b2backupv1.SourceTypeCNPG}
}

func succeeded(job string, trigger b2backupv1.BackupRunTrigger, start time.Duration, took time.Duration) b2backupv1.BackupRun {
	return b2backupv1.BackupRun{JobName: job, Trigger: trigger, Result: b2backupv1.BackupRunSucceeded,
		StartTime: at(start), CompletionTime: at(start + took)}
}

func repository(ns string) *b2backupv1.Repository {
	return &b2backupv1.Repository{
		Namespace: ns, Name: nameRestic, UID: types.UID(ns + "-repo"),
		Status: b2backupv1.RepositoryStatus{
			BucketName: "mcswain-kubernetes-backups", Path: "k8s/" + ns, Adopted: true, Initialized: true,
			Endpoint: "https://s3.us-east-005.backblazeb2.com", ApplicationKeyID: "005000000000000000000011",
			CurrentUsage: "2.4 MiB", UsageTime: at(-48 * time.Hour),
			Conditions: []metav1.Condition{
				{Type: reasonReady, Status: metav1.ConditionTrue, Reason: "FoundExisting",
					Message: "found the existing restic repository, nothing was created"},
				{Type: "LifecycleRule", Status: metav1.ConditionTrue, Reason: "CoveredByExistingRule",
					Message: "covered by the k8s/ rule: hidden files are deleted after 30 days"},
			},
		},
	}
}

func cronJob(sb *b2backupv1.ScheduledBackup) *batchv1.CronJob {
	return &batchv1.CronJob{
		Namespace: sb.Namespace, Name: backup.CronJobName(sb),
		UID: types.UID(sb.Namespace + "-cj"), Labels: map[string]string{labelManagedBy: operatorManaged},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "b2backup.mcswain.dev/v1", Kind: "ScheduledBackup",
			Name: sb.Name, UID: sb.UID, Controller: ptr.To(true)}},
		Spec: batchv1.CronJobSpec{Schedule: sb.Status.EffectiveSchedule, TimeZone: ptr.To(testZone),
			Suspend: ptr.To(sb.Spec.Suspend)},
	}
}

func backupJob(sb *b2backupv1.ScheduledBackup, name string, start time.Duration, manual bool) *batchv1.Job {
	labels := map[string]string{labelManagedBy: operatorManaged, "app.kubernetes.io/name": sb.Name}
	owner := metav1.OwnerReference{APIVersion: "batch/v1", Kind: "CronJob", Name: backup.CronJobName(sb),
		UID: types.UID(sb.Namespace + "-cj"), Controller: ptr.To(true)}
	if manual {
		labels[backup.LabelTrigger] = backup.TriggerManual
		owner = metav1.OwnerReference{APIVersion: "b2backup.mcswain.dev/v1", Kind: "ScheduledBackup", Name: sb.Name,
			UID: sb.UID, Controller: ptr.To(true)}
	}
	return &batchv1.Job{
		Namespace: sb.Namespace, Name: name, Labels: labels,
		OwnerReferences: []metav1.OwnerReference{owner}, CreationTimestamp: *at(start),
		Status: batchv1.JobStatus{StartTime: at(start), Active: 1},
	}
}

func failedPod(job *batchv1.Job, exit int32) *corev1.Pod {
	return &corev1.Pod{
		Namespace: job.Namespace, Name: job.Name + "-x1",
		Labels: map[string]string{labelManagedBy: operatorManaged, batchv1.JobNameLabel: job.Name},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "restic", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit}}}}},
	}
}

// cluster is the example cluster the frames are drawn from.
func cluster() []client.Object {
	var objs []client.Object
	add := func(sb *b2backupv1.ScheduledBackup) *b2backupv1.ScheduledBackup {
		objs = append(objs, sb, repository(sb.Namespace), cronJob(sb))
		return sb
	}

	apprise := add(scheduled("apprise", "36 1 * * *", files("/config")))
	apprise.Status.History = []b2backupv1.BackupRun{succeeded("restic-backup-1", b2backupv1.BackupTriggerScheduled,
		-17*time.Hour, 20*time.Second)}

	gitea := add(scheduled(nsGitea, "28 7 * * *", files("/data"), dbSource()))
	gitea.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineCNPG}
	gitea.Status.History = []b2backupv1.BackupRun{succeeded("restic-backup-2", b2backupv1.BackupTriggerScheduled,
		-11*time.Hour, 40*time.Second)}
	gitea.Status.LastTriggerJob, gitea.Status.LastTriggerTime = "restic-manual-x8k2qp", at(-135*time.Second)
	objs = append(objs, backupJob(gitea, "restic-manual-x8k2qp", -134*time.Second, true))

	kosync := add(scheduled(nsKosync, "15 9 * * *", dbSource()))
	kosync.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineCNPG}
	kosync.Spec.Volumes = nil
	kosync.Status.History = []b2backupv1.BackupRun{
		succeeded("restic-backup-29342155", b2backupv1.BackupTriggerScheduled, -9*time.Hour-35*time.Minute, 24*time.Second),
		succeeded("restic-manual-tmrg1h", b2backupv1.BackupTriggerManual, -25*time.Hour-52*time.Minute, 25*time.Second),
		succeeded("restic-backup-29340715", b2backupv1.BackupTriggerScheduled, -33*time.Hour-35*time.Minute, 22*time.Second),
		succeeded("restic-backup-29339275", b2backupv1.BackupTriggerScheduled, -57*time.Hour-35*time.Minute, 23*time.Second),
		{JobName: "restic-backup-29337835", Trigger: b2backupv1.BackupTriggerScheduled,
			Result: b2backupv1.BackupRunFailed, StartTime: at(-81*time.Hour - 35*time.Minute),
			CompletionTime: at(-81*time.Hour - 35*time.Minute + 62*time.Second)},
		succeeded("restic-backup-29336395", b2backupv1.BackupTriggerScheduled, -105*time.Hour-35*time.Minute, 21*time.Second),
	}

	minecraft := add(scheduled(nsMinecraft, "7 15 * * *", files("/data", "/world")))
	minecraft.Spec.Volumes = append(minecraft.Spec.Volumes, b2backupv1.VolumeSpec{ExistingClaim: "world",
		MountPath: "/world"})
	minecraft.Status.History = []b2backupv1.BackupRun{
		{JobName: "restic-backup-29342505", Trigger: b2backupv1.BackupTriggerScheduled,
			Result: b2backupv1.BackupRunFailed, StartTime: at(-3*time.Hour - 43*time.Minute),
			CompletionTime: at(-3*time.Hour - 42*time.Minute)},
		succeeded("restic-backup-29341065", b2backupv1.BackupTriggerScheduled, -27*time.Hour-43*time.Minute, 3*time.Minute),
	}
	failed := backupJob(minecraft, "restic-backup-29342505", -3*time.Hour-43*time.Minute, false)
	failed.Status = batchv1.JobStatus{StartTime: at(-3*time.Hour - 43*time.Minute), Failed: 1,
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", LastTransitionTime: *at(-3*time.Hour - 42*time.Minute)}}}
	objs = append(objs, failed, failedPod(failed, 3))

	objs = append(objs, &corev1.PersistentVolumeClaim{
		Namespace: nsVault, Name: claimData,
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("longhorn-nvme"),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi")}}},
	}, &corev1.Pod{
		Namespace: nsVault, Name: "vaultwarden-6c9d8f7b5-x2x4k",
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: claimData,
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimData}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})
	vault := add(scheduled(nsVault, "5 3 * * *", files("/data"), dbSource()))
	vault.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineCNPG}
	vault.Status.History = []b2backupv1.BackupRun{succeeded("restic-backup-3", b2backupv1.BackupTriggerScheduled,
		-15*time.Hour, 30*time.Second)}

	wx := add(scheduled("wx", "57 10 * * *"))
	wx.Spec.Script = "restic backup /data\n"
	wx.Spec.Volumes = nil
	wx.Spec.Suspend = true
	wx.Status.Conditions[0].Reason = "Suspended"
	wx.Status.History = []b2backupv1.BackupRun{succeeded("restic-backup-4", b2backupv1.BackupTriggerScheduled,
		-7*time.Hour, 30*time.Second)}
	return objs
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(s))
	utilruntime.Must(batchv1.AddToScheme(s))
	utilruntime.Must(b2backupv1.AddToScheme(s))
	return s
}

// fakeBackend serves the example cluster and records the actions taken.
type fakeBackend struct {
	mu      sync.Mutex
	client  client.Client
	ns      string
	denied  map[string]bool
	calls   []string
	restore []RestoreRequest

	podGate   chan struct{}
	snapshots []Snapshot
	nodes     map[string][]Node
	logs      map[string]string
	dryRun    string
	checks    []Check
	targets   map[Target]string
	closed    int

	checkBlocks bool
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(cluster()...).Build()
	return &fakeBackend{
		client: c, ns: nsVault, denied: map[string]bool{},
		snapshots: exampleSnapshots(), nodes: map[string][]Node{snapLatest: exampleNodes()},
		logs: map[string]string{nsGitea + "/restic-manual-x8k2qp": giteaLog},
		dryRun: "restored  /data/db.sqlite3 with size 6.8 MiB\nupdated   /data/db.sqlite3-wal with size 412 KiB\n" +
			"restored  /data/icon_cache/github.png with size 3 KiB\nunchanged /data/config.json with size 1.1 KiB\n" +
			"unchanged /data/rsa_key.pem with size 1.7 KiB\n",
		checks: exampleChecks(),
	}
}

const giteaLog = `2026-10-11T23:48:02Z + restic backup --retry-lock=5m0s --tag=files /data
2026-10-11T23:48:03Z using parent snapshot 5d1e09ab
2026-10-11T23:48:03Z open repository
2026-10-11T23:48:05Z lock repository
2026-10-11T23:48:06Z load index files
2026-10-11T23:48:09Z start scan on [/data]
2026-10-11T23:48:09Z start backup on [/data]
2026-10-11T23:48:41Z scan finished in 32.118s: 41822 files, 1.937 GiB
2026-10-11T23:49:20Z warning: /data/gitea/indexers/issues.bleve/store: file changed during read, retrying
2026-10-11T23:50:10Z [2:07] 61.00%  25511 files 1.182 GiB, total 41822 files 1.937 GiB, 0 errors ETA 1:20
`

func exampleSnapshots() []Snapshot {
	snap := func(id string, ago time.Duration, size uint64, tag string, paths ...string) Snapshot {
		return Snapshot{ID: id + "00000000000000000000000000000000000000000000000000000000", ShortID: id,
			Time: testNow.Add(-ago), Paths: paths, Hostname: nsVault, Tags: []string{tag}, Size: size, HasSize: true}
	}
	const mib = 1 << 20
	day := 24 * time.Hour
	return []Snapshot{
		snap(snapLatest, 15*time.Hour, 48*mib+mib/5, tagFiles, "/data"),
		snap("b81c440e", 15*time.Hour+day, 48*mib+mib/5, tagFiles, "/data"),
		snap("20f3a991", 15*time.Hour+2*day, 48*mib+mib/10, tagFiles, "/data"),
		snap("c71d0e52", 15*time.Hour+3*day, 48*mib+mib/10, tagFiles, "/data"),
		snap("3b47d1c0", 193*day, 31*mib, tagFiles, "/data"),
		snap("1e77c0d4", 15*time.Hour, 2*mib, tagDB, "/vaultwarden.dmp"),
		snap("a02f6d19", 15*time.Hour+day, 3*mib, tagDB, "/vaultwarden"),
	}
}

func exampleNodes() []Node {
	dir := func(p string) Node { return Node{Path: p, Type: nodeDir, Mode: os.ModeDir | 0o755} }
	file := func(p string, size uint64, mode os.FileMode) Node {
		return Node{Path: p, Type: "file", Size: size, Mode: mode, UID: 1000}
	}
	nodes := make([]Node, 0, 13)
	nodes = append(nodes,
		dir("/data"), dir("/data/attachments"), dir("/data/icon_cache"), dir("/data/sends"),
		file("/data/config.json", 1126, 0o600), file("/data/db.sqlite3", 7130317, 0o644),
		file("/data/rsa_key.pem", 1700, 0o600), file("/data/rsa_key.pub.pem", 451, 0o644),
	)
	for i := range 3 {
		nodes = append(nodes, file(fmt.Sprintf("/data/attachments/a%d", i), 100, 0o644))
	}
	nodes = append(nodes, file("/data/icon_cache/github.png", 3000, 0o644), file("/data/sends/s1", 10, 0o600))
	return nodes
}

func exampleChecks() []Check {
	return []Check{
		{Level: LevelOK, Summary: "backup is ready"},
		{Level: LevelOK, Summary: `repository "restic" is ready and initialized`},
		{Level: LevelOK, Summary: `credentials Secret "restic-credentials" has all 5 keys, application key 0050…0011`},
		{Level: LevelOK, Summary: `CronJob "restic-backup" is owned by this ScheduledBackup`},
		{Level: LevelWarn, Summary: `cache PVC "restic-cache" is Pending, not Bound`,
			Detail: []string{"A pending claim will block the backup pod from starting."}},
		{Level: LevelFail, Summary: "the most recent run (3h ago) failed", Detail: []string{"Run: b2bk logs sb/restic"},
			Next: []Step{StepLogs, StepBackup}},
	}
}

func (b *fakeBackend) record(call string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, call)
}

func (b *fakeBackend) recorded() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *fakeBackend) Context() string                { return testContext }
func (b *fakeBackend) Namespace() (string, error)     { return b.ns, nil }
func (b *fakeBackend) Reader() (client.Reader, error) { return b.client, nil }

func (b *fakeBackend) Clientset() (kubernetes.Interface, error) {
	cs := k8sfake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(a k8stesting.Action) (bool, runtime.Object, error) {
			review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
			attrs := review.Spec.ResourceAttributes
			what := attrs.Verb + " " + attrs.Resource
			if attrs.Subresource != "" {
				what += "/" + attrs.Subresource
			}
			review.Status.Allowed = !b.denied[what]
			return true, review, nil
		})
	return cs, nil
}

func (b *fakeBackend) Watch(_ context.Context, _ string, _ []Kind, _ func()) (client.Reader, error) {
	return b.client, nil
}

func (b *fakeBackend) Operator(context.Context) OperatorInfo {
	return OperatorInfo{Version: "v0.1.4", Ready: true, Found: true}
}

func (b *fakeBackend) BackupNow(_ context.Context, ns, name string) (string, error) {
	b.record("backup " + ns + "/" + name)
	return "triggered backup of scheduledbackup/" + name + "\n  the operator will create job/restic-manual-abc\n", nil
}

func (b *fakeBackend) SetSuspend(_ context.Context, ns, name string, suspend bool) (string, error) {
	b.record(fmt.Sprintf("suspend %s/%s %t", ns, name, suspend))
	return "scheduledbackup/" + name + " suspended\n", nil
}

func (b *fakeBackend) Cancel(_ context.Context, ns, name string) (string, error) {
	b.record("cancel " + ns + "/" + name)
	return "cancelled job/x\n", nil
}

func (b *fakeBackend) Doctor(_ context.Context, ns, name string) ([]Check, error) {
	b.record("doctor " + ns + "/" + name)
	return b.checks, nil
}

func (b *fakeBackend) Logs(_ context.Context, ns, job string, _ bool, w io.Writer) error {
	b.record("logs " + ns + "/" + job)
	_, err := io.WriteString(w, b.logs[ns+"/"+job])
	return err
}

func (b *fakeBackend) LastLogLine(_ context.Context, _, job string) string {
	if job == "restic-backup-29342505" {
		return "/data/world/session.lock: permission denied"
	}
	return ""
}

func (b *fakeBackend) Check(ctx context.Context, ns, name, readData string, w io.Writer) error {
	b.record("check " + ns + "/" + name + " " + readData)
	if b.checkBlocks {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		b.record("check cleaned up")
		return ctx.Err()
	}
	_, err := io.WriteString(w, "using temporary cache\nno errors were found\n")
	return err
}

type fakePod struct {
	b    *fakeBackend
	name string
}

func (p *fakePod) Name() string { return p.name }

func (p *fakePod) Exec(context.Context, []string, io.Writer) error { return nil }

func (p *fakePod) Close() {
	p.b.mu.Lock()
	defer p.b.mu.Unlock()
	p.b.closed++
}

func (b *fakeBackend) OpenPod(ctx context.Context, ns, name string, progress io.Writer) (Pod, error) {
	b.record("open " + ns + "/" + name)
	if b.podGate != nil {
		_, _ = io.WriteString(progress, "still waiting: restic-b2bk-browse-7qx2m-abcde is ContainerCreating (15s of 10m0s)\n")
		select {
		case <-b.podGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &fakePod{b: b, name: "restic-b2bk-browse-7qx2m"}, nil
}

func (b *fakeBackend) Snapshots(context.Context, Pod, string, string) ([]Snapshot, error) {
	return b.snapshots, nil
}

func (b *fakeBackend) Files(_ context.Context, _ Pod, id string) ([]Node, error) {
	for _, s := range b.snapshots {
		if s.ID == id {
			return b.nodes[s.ShortID], nil
		}
	}
	return nil, fmt.Errorf("no snapshot %s", id)
}

func (b *fakeBackend) Diff(_ context.Context, _ Pod, from, to string, w io.Writer) error {
	b.record("diff " + from[:8] + " " + to[:8])
	_, err := io.WriteString(w, "comparing snapshot "+from[:8]+" to "+to[:8]+":\n\nM    /data/db.sqlite3\n")
	return err
}

func (b *fakeBackend) Download(_ context.Context, _ Pod, req DownloadRequest, w io.Writer) error {
	b.record("download " + req.Path + " " + req.Dir)
	_, err := io.WriteString(w, "downloading "+req.Path+"\n")
	return err
}

func (b *fakeBackend) RestoreTargets(context.Context, string, string, string) map[Target]string {
	if b.targets != nil {
		return b.targets
	}
	return map[Target]string{TargetDatabase: "needs a db snapshot; switch the tag to db with tab in snapshots"}
}

func (b *fakeBackend) Restore(_ context.Context, req RestoreRequest, w io.Writer) error {
	b.mu.Lock()
	b.restore = append(b.restore, req)
	b.mu.Unlock()
	if req.DryRun {
		_, err := io.WriteString(w, b.dryRun)
		return err
	}
	_, err := io.WriteString(w, "restoring 5d1e09ab:/data into /data\n")
	return err
}

func (b *fakeBackend) restores() []RestoreRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]RestoreRequest(nil), b.restore...)
}
