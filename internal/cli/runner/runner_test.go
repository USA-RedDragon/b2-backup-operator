package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"k8s.io/apimachinery/pkg/util/validation"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const (
	testNS     = "prod"
	testName   = "web-files"
	testImage  = "ghcr.io/usa-reddragon/b2-backup-operator-restic:test"
	testRepo   = "store"
	testDBName = "app"
)

var testCommand = []string{"restic", "snapshots"}

func fixture(t *testing.T, mutate func(*b2backupv1.ScheduledBackup)) (*Runner, *b2backupv1.ScheduledBackup) {
	t.Helper()

	sb := &b2backupv1.ScheduledBackup{
		Namespace: testNS, Name: testName, UID: "uid-sb",
		Spec: b2backupv1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: testRepo},
			Schedule:      "@hourly",
			Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
		},
	}
	if mutate != nil {
		mutate(sb)
	}
	repo := &b2backupv1.Repository{Namespace: testNS, Name: testRepo}

	cj, err := backup.BuildCronJob(sb, repo, backup.Config{Image: testImage})
	if err != nil {
		t.Fatalf("BuildCronJob: %v", err)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		b2backupv1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objs := []client.Object{sb, repo, cj}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Runner{Client: c}, sb
}

func containerOf(t *testing.T, job *batchv1.Job) *corev1.Container {
	t.Helper()
	c := findContainer(&job.Spec.Template.Spec)
	if c == nil {
		t.Fatal("no restic container in the built job")
	}
	return c
}

func hasVolume(job *batchv1.Job, name string) bool {
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func TestBuildInheritsTheOperatorsConfiguration(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	c := containerOf(t, job)
	if c.Image != testImage {
		t.Errorf("image = %q, want the CronJob's %q", c.Image, testImage)
	}
	if strings.Join(c.Command, " ") != strings.Join(testCommand, " ") {
		t.Errorf("command = %v, want the override", c.Command)
	}

	if len(c.EnvFrom) == 0 || c.EnvFrom[0].SecretRef == nil {
		t.Errorf("envFrom was not inherited: %+v", c.EnvFrom)
	}

	if envOf(c, "RESTIC_HOST") == nil {
		t.Error("RESTIC_HOST was not inherited")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
}

func TestBuildIsNotLabelledAsOperatorManaged(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		if got := labels[labelManagedBy]; got != ManagedByValue {
			t.Errorf("managed-by = %q, want %q", got, ManagedByValue)
		}
	}
}

func TestBuildUsesScratchCacheByDefault(t *testing.T) {
	r, sb := fixture(t, nil)
	ctx := context.Background()

	job, err := r.Build(ctx, sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cache := volumeOf(job, cacheVolume)
	if cache == nil {
		t.Fatal("the cache volume was dropped; RESTIC_CACHE_DIR would break")
	}
	if cache.EmptyDir == nil || cache.PersistentVolumeClaim != nil {
		t.Errorf("cache volume = %+v, want an emptyDir", cache)
	}

	shared, err := r.Build(ctx, sb, Options{Command: testCommand, MountCache: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if volumeOf(shared, cacheVolume).PersistentVolumeClaim == nil {
		t.Error("--mount-cache should use the real claim")
	}
}

func TestBuildDataVolumeAndAffinity(t *testing.T) {
	withVolume := func(sb *b2backupv1.ScheduledBackup) {
		sb.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: "app-data", ReadOnly: true}}
		sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}
	}
	r, sb := fixture(t, withVolume)
	ctx := context.Background()

	bare, err := r.Build(ctx, sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if hasVolume(bare, backup.DataVolumeName(0)) {
		t.Error("the data volume should be dropped unless requested")
	}
	if aff := bare.Spec.Template.Spec.Affinity; aff != nil && aff.PodAffinity != nil {
		t.Errorf("pod affinity should be dropped with the data volume: %+v", aff.PodAffinity)
	}

	restore, err := r.Build(ctx, sb, Options{Command: testCommand, MountData: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	data := volumeOf(restore, backup.DataVolumeName(0))
	if data == nil || data.PersistentVolumeClaim == nil {
		t.Fatal("--mount-data should attach the source claim")
	}
	if data.PersistentVolumeClaim.ReadOnly {
		t.Error("the source claim must be writable for a restore")
	}
	for _, m := range containerOf(t, restore).VolumeMounts {
		if m.Name == backup.DataVolumeName(0) && m.ReadOnly {
			t.Error("the data mount must be writable for a restore")
		}
	}
	aff := restore.Spec.Template.Spec.Affinity
	if aff == nil || aff.PodAffinity == nil ||
		aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Error("the hard affinity must be kept when the claim is mounted")
	}
}

func TestDropDataKeepsASoftAffinity(t *testing.T) {
	pod := &corev1.PodSpec{Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
			{TopologyKey: "kubernetes.io/hostname"},
		},
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
			{Weight: 1, PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname"}},
		},
	}}}
	dropData(pod, &corev1.Container{})

	aff := pod.Affinity.PodAffinity
	if aff == nil {
		t.Fatal("a soft affinity must survive dropping the data volume")
	}
	if aff.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Error("the hard affinity pins the pod to the app and must be cleared")
	}
}

func TestBuildKeepsMariaDBClientLabel(t *testing.T) {
	r, sb := fixture(t, func(sb *b2backupv1.ScheduledBackup) {
		sb.Spec.Database = &b2backupv1.DatabaseSpec{
			Engine: b2backupv1.DatabaseEngineMariaDB,
			Host:   "mariadb", Name: "app", User: "root",
		}
		sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeMariaDB}}
	})

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := job.Spec.Template.Labels["mariadb-client"]; got != "true" {
		t.Errorf("mariadb-client = %q, want true", got)
	}
}

func TestBuildInteractiveIdles(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{TTY: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := containerOf(t, job)
	if strings.Join(c.Command, " ") != strings.Join(idleCommand, " ") {
		t.Errorf("command = %v, want the idle command so exec can attach", c.Command)
	}
	if !c.Stdin || !c.TTY {
		t.Error("an interactive pod needs stdin and tty")
	}
}

func TestBuildBoundsOnlyTheIdlingJob(t *testing.T) {
	r, sb := fixture(t, nil)
	ctx := context.Background()

	batch, err := r.Build(ctx, sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if batch.Spec.ActiveDeadlineSeconds != nil {
		t.Errorf("a batch run must not be deadlined mid-write: %v", *batch.Spec.ActiveDeadlineSeconds)
	}

	idle, err := r.Build(ctx, sb, Options{TTY: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := int64(interactiveDeadline.Seconds())
	if idle.Spec.ActiveDeadlineSeconds == nil || *idle.Spec.ActiveDeadlineSeconds != want {
		t.Errorf("activeDeadlineSeconds = %v, want %d so TTL can reap the idle pod",
			idle.Spec.ActiveDeadlineSeconds, want)
	}

	kept, err := r.Build(ctx, sb, Options{TTY: true, Keep: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if kept.Spec.ActiveDeadlineSeconds != nil {
		t.Error("--keep exists to leave the pod running; it must not be deadlined")
	}
}

func TestBuildIsOwnedButNotControlledByTheBackup(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if owner := metav1.GetControllerOf(job); owner != nil {
		t.Errorf("a controller reference would make the operator adopt this Job: %+v", owner)
	}

	refs := job.OwnerReferences
	if len(refs) != 1 {
		t.Fatalf("ownerReferences = %+v, want one so the Job is collected with the backup", refs)
	}
	if refs[0].Kind != "ScheduledBackup" || refs[0].Name != sb.Name || refs[0].UID != sb.UID {
		t.Errorf("ownerReference = %+v, want the ScheduledBackup", refs[0])
	}
	if refs[0].BlockOwnerDeletion != nil && *refs[0].BlockOwnerDeletion {
		t.Error("blockOwnerDeletion needs a finalizers grant the CLI does not have")
	}
}

func TestBuildWithoutACronJob(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := b2backupv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sb := &b2backupv1.ScheduledBackup{Namespace: testNS, Name: testName}
	r := &Runner{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sb).Build()}

	_, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if !errors.Is(err, ErrNoCronJob) {
		t.Fatalf("expected ErrNoCronJob, got %v", err)
	}
	if !strings.Contains(err.Error(), "b2bk doctor") {
		t.Errorf("the error should point somewhere useful: %v", err)
	}
}

func TestJobNameFitsAndIsUnique(t *testing.T) {
	long := &b2backupv1.ScheduledBackup{Name: strings.Repeat("a", 80)}

	const draws = 200
	seen := make(map[string]struct{}, draws)
	for range draws {
		name := jobName(long, "snapshots")
		if len(name) > 52 {
			t.Fatalf("job name is %d chars, want <= 52: %q", len(name), name)
		}
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			t.Fatalf("job name %q is not a valid Job name: %v", name, errs)
		}
		seen[name] = struct{}{}
	}
	if len(seen) < draws-10 {
		t.Errorf("jobName returned %d distinct names in %d calls; the suffix is not being "+
			"redrawn, and two concurrent runs would fight over one Job", len(seen), draws)
	}

	if got := jobName(long, strings.Repeat("p", 60)); got == "" {
		t.Error("a purpose longer than the name budget must not panic or empty the name")
	}
}

func TestBlockedReason(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: "manifest unknown",
			}},
		}},
	}}
	if got := blockedReason(pod); !strings.Contains(got, "ImagePullBackOff") {
		t.Errorf("blockedReason = %q", got)
	}

	starting := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ContainerCreating",
			}},
		}},
	}}
	if got := blockedReason(starting); got != "" {
		t.Errorf("a pod that is merely starting must not be reported: %q", got)
	}
}

func volumeOf(job *batchv1.Job, name string) *corev1.Volume {
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == name {
			return &job.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

func envOf(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func TestPreflightCatchesAMissingSecret(t *testing.T) {
	r, sb := fixture(t, func(sb *b2backupv1.ScheduledBackup) {
		sb.Spec.Database = &b2backupv1.DatabaseSpec{
			Engine: b2backupv1.DatabaseEngineCNPG, Host: "postgresql-rw", Name: testDBName, User: testDBName,
		}
	})

	if err := r.Client.Create(context.Background(),
		&corev1.Secret{Namespace: testNS, Name: testRepo + "-credentials"}); err != nil {
		t.Fatal(err)
	}

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand, MountDatabase: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	err = r.Preflight(context.Background(), job)
	if !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("expected ErrMissingDependency, got %v", err)
	}

	if !strings.Contains(err.Error(), "postgresql-app") {
		t.Errorf("error should name the missing Secret: %v", err)
	}
}

func TestPreflightChecksEnvFromSecrets(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	err = r.Preflight(context.Background(), job)
	if !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("expected ErrMissingDependency for the credentials Secret, got %v", err)
	}
}

func TestPreflightPassesWhenDependenciesExist(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := r.Client.Create(context.Background(),
		&corev1.Secret{Namespace: testNS, Name: testRepo + "-credentials"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Create(context.Background(),
		&corev1.PersistentVolumeClaim{Namespace: testNS, Name: testName + "-cache"}); err != nil {
		t.Fatal(err)
	}

	if err := r.Preflight(context.Background(), job); err != nil {
		t.Errorf("preflight should pass with dependencies present: %v", err)
	}
}

func TestBlockedReasonUnschedulable(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason:  corev1.PodReasonUnschedulable,
			Message: "0/3 nodes are available: 3 node(s) had volume node affinity conflict.",
		}},
	}}
	if got := blockedReason(pod); !strings.Contains(got, "Unschedulable") {
		t.Errorf("blockedReason = %q", got)
	}
}

func TestBuildOnlyMountsDatabaseCredentialsWhenAsked(t *testing.T) {
	mutate := func(sb *b2backupv1.ScheduledBackup) {
		sb.Spec.Database = &b2backupv1.DatabaseSpec{
			Engine: b2backupv1.DatabaseEngineCNPG, Host: "postgresql-rw",
			Name: testDBName, User: testDBName,
		}
	}
	r, sb := fixture(t, mutate)
	ctx := context.Background()

	bare, err := r.Build(ctx, sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if hasVolume(bare, dbVolume) {
		t.Error("database credentials mounted into a run that does not use them")
	}

	restore, err := r.Build(ctx, sb, Options{Command: testCommand, MountDatabase: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !hasVolume(restore, dbVolume) {
		t.Error("--to-database needs the credentials mounted")
	}
}

func jobWithPod(t *testing.T, pod *corev1.Pod) (*Runner, *batchv1.Job) {
	t.Helper()

	job := &batchv1.Job{Namespace: testNS, Name: testName + "-b2bk-shell-abcde"}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	objs := []client.Object{job}
	if pod != nil {
		pod.Namespace = testNS
		pod.Labels = map[string]string{jobNameLabel: job.Name}
		objs = append(objs, pod)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Runner{Client: c}, job
}

func TestWaitForPodReturnsTheRunningPod(t *testing.T) {
	r, job := jobWithPod(t, &corev1.Pod{
		Name:   "running",
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})

	pod, err := r.WaitForPod(context.Background(), job)
	if err != nil {
		t.Fatalf("WaitForPod: %v", err)
	}
	if pod.Name != "running" {
		t.Errorf("pod = %q, want the job's pod", pod.Name)
	}
}

func TestWaitForPodNamesTheBudgetAndReportsProgress(t *testing.T) {
	t.Parallel()
	r, job := jobWithPod(t, &corev1.Pod{
		Name: "cold-pull",
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ContainerCreating",
				}},
			}},
		},
	})

	var progress strings.Builder
	budget := 1500 * time.Millisecond
	r.PodStartTimeout = budget
	r.Progress = &progress
	_, err := r.WaitForPod(context.Background(), job)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline, got %v", err)
	}
	for _, want := range []string{budget.String(), "--pod-start-timeout", "cold-pull is ContainerCreating"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}

	if got := progress.String(); !strings.Contains(got, "cold-pull is ContainerCreating") {
		t.Errorf("a long wait must not be a silent one; progress was %q", got)
	}
}

func TestWaitForPodDefaultsToTenMinutes(t *testing.T) {
	if DefaultPodStartTimeout < 10*time.Minute {
		t.Errorf("DefaultPodStartTimeout = %s, too short for a cold image pull", DefaultPodStartTimeout)
	}
}

func runningJobPod(t *testing.T, r *Runner, failed bool) {
	t.Helper()

	go func() {
		ctx := context.Background()
		for range 200 {
			var jobs batchv1.JobList
			if err := r.Client.List(ctx, &jobs, client.InNamespace(testNS)); err != nil {
				return
			}
			for i := range jobs.Items {
				job := &jobs.Items[i]
				if job.Labels[labelManagedBy] != ManagedByValue {
					continue
				}
				pod := &corev1.Pod{
					Namespace: testNS, Name: job.Name + "-0",
					Labels: map[string]string{jobNameLabel: job.Name},
				}
				_ = r.Client.Create(ctx, pod)
				pod.Status.Phase = corev1.PodRunning
				_ = r.Client.Status().Update(ctx, pod)
				if failed {
					job.Status.Failed = 1
				} else {
					job.Status.Succeeded = 1
				}
				_ = r.Client.Status().Update(ctx, job)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func runnableFixture(t *testing.T) (*Runner, *b2backupv1.ScheduledBackup) {
	t.Helper()

	r, sb := fixture(t, nil)
	r.Clientset = fakeclientset.NewSimpleClientset()

	ctx := context.Background()
	for _, obj := range []client.Object{
		&corev1.Secret{Namespace: testNS, Name: testRepo + "-credentials"},
		&corev1.PersistentVolumeClaim{Namespace: testNS, Name: testName + "-cache"},
	} {
		if err := r.Client.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	return r, sb
}

var errBrokenPipe = errors.New("broken pipe")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errBrokenPipe }

func TestRunReportsTheJobsResultNotTheLogStream(t *testing.T) {
	t.Parallel()
	r, sb := runnableFixture(t)
	runningJobPod(t, r, true)

	r.PodStartTimeout = 10 * time.Second
	opts := Options{Command: testCommand}
	err := r.Run(context.Background(), sb, opts, failingWriter{}, 10*time.Second)
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("a log-streaming hiccup masked the job result: %v", err)
	}
}

func TestRunStillReportsAStreamErrorWhenTheJobSucceeded(t *testing.T) {
	t.Parallel()
	r, sb := runnableFixture(t)
	runningJobPod(t, r, false)

	r.PodStartTimeout = 10 * time.Second
	opts := Options{Command: testCommand}
	err := r.Run(context.Background(), sb, opts, failingWriter{}, 10*time.Second)
	if !errors.Is(err, errBrokenPipe) {
		t.Errorf("the job succeeded, so the stream error should stand: %v", err)
	}
}

const testKeySecret = "app-credentials"

func preflightWithDependencies(t *testing.T, extra ...client.Object) error {
	t.Helper()

	r, sb := fixture(t, nil)
	ctx := context.Background()

	objs := []client.Object{
		&corev1.Secret{Namespace: testNS, Name: testRepo + "-credentials"},
		&corev1.PersistentVolumeClaim{Namespace: testNS, Name: testName + "-cache"},
	}
	for _, obj := range append(objs, extra...) {
		if err := r.Client.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	job, err := r.Build(ctx, sb, Options{Command: testCommand, MountCache: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := containerOf(t, job)
	c.Env = append(c.Env, corev1.EnvVar{
		Name: "API_KEY",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			Name: testKeySecret,
			Key:  "api-key",
		}},
	})
	return r.Preflight(ctx, job)
}

func TestPreflightChecksSecretKeyRefEnv(t *testing.T) {
	err := preflightWithDependencies(t)
	if !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("expected ErrMissingDependency for API_KEY's Secret, got %v", err)
	}
	if !strings.Contains(err.Error(), testKeySecret) {
		t.Errorf("the error should name the missing Secret: %v", err)
	}
}

func TestPreflightChecksTheKeyWithinTheSecret(t *testing.T) {
	err := preflightWithDependencies(t, &corev1.Secret{Namespace: testNS, Name: testKeySecret})
	if !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("expected ErrMissingDependency for the absent key, got %v", err)
	}
	if !strings.Contains(err.Error(), "api-key") {
		t.Errorf("the error should name the missing key: %v", err)
	}
}

func TestPreflightPassesWithTheKeyPresent(t *testing.T) {
	err := preflightWithDependencies(t, &corev1.Secret{
		Namespace: testNS, Name: testKeySecret,
		Data: map[string][]byte{"api-key": []byte("hunter2")},
	})
	if err != nil {
		t.Errorf("preflight should pass once the key exists: %v", err)
	}
}

func TestOnlyRestoreJobsGetTheRestoreCapabilities(t *testing.T) {
	t.Parallel()
	r, sb := fixture(t, nil)
	tests := []struct {
		name string
		opts Options
		want []corev1.Capability
	}{
		{"a backup's own capability", Options{Command: testCommand}, []corev1.Capability{backup.BackupCapability}},
		{"a restore", Options{Command: testCommand, Restore: true},
			[]corev1.Capability{"DAC_OVERRIDE", "CHOWN", "FOWNER", "FSETID"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			job, err := r.Build(context.Background(), sb, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			sc := containerOf(t, job).SecurityContext
			if sc == nil || sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Add, tt.want) ||
				!slices.Contains(sc.Capabilities.Drop, "ALL") {
				t.Errorf("capabilities = %+v, want %v added to ALL dropped", sc.Capabilities, tt.want)
			}
		})
	}
}
