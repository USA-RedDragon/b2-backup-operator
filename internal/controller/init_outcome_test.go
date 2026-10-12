package controller

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const (
	reasonCreated       = "Created"
	reasonFoundExisting = "FoundExisting"
	caseEmptyPath       = "an empty path"
)

func finishInitPod(t *testing.T, c client.Client, message string, owned bool) {
	t.Helper()
	job := initJob(t, c)
	pod := &corev1.Pod{
		Namespace: testNS, Name: job.Name + "-abcde",
		Labels: map[string]string{batchv1.JobNameLabel: job.Name, batchv1.ControllerUidLabel: string(job.UID)},
	}
	if owned {
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name,
			UID: job.UID, Controller: ptr.To(true)}}
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "init",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: message}},
	}}}
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	succeedInitJob(t, c)
}

func TestInitSaysWhetherItCreatedOrFoundTheRepository(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		existing   bool
		message    string
		owned      bool
		wantReason string
		wantSays   string
		wantNot    string
	}{
		{name: caseEmptyPath, message: initCreated + "\n", owned: true, wantReason: reasonCreated,
			wantSays: "Created a new restic repository", wantNot: "existing"},
		{name: "an adopted repository", existing: true, message: initFound + "\n", owned: true, wantReason: reasonFoundExisting,
			wantSays: "Found the existing restic repository", wantNot: reasonCreated},
		{name: "a pod the Job does not own", existing: true, message: initCreated + "\n", wantReason: "Initialized",
			wantSays: "Opened the restic repository", wantNot: reasonCreated},
		{name: "a pod already gone", existing: true, wantReason: "Initialized",
			wantSays: "Opened the restic repository", wantNot: reasonCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			bucket := f.AddBucket(sharedBucket)
			if tt.existing {
				f.PutFile(bucket.BucketID, configPath(myappPath), 1)
			}
			r, c := newHarness(t, f, prefixRepo(usePasswordSeed), seedSecretFixture())
			reconcileN(t, r, 1)
			if tt.message != "" {
				finishInitPod(t, c, tt.message, tt.owned)
			} else {
				succeedInitJob(t, c)
			}
			reconcileN(t, r, 2)

			cond := condition(t, c, b2backupv1.RepositoryConditionInitialized)
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tt.wantReason ||
				!strings.Contains(cond.Message, tt.wantSays) {
				t.Errorf("Initialized = %+v, want True/%s saying %q", cond, tt.wantReason, tt.wantSays)
			}
			recorded := recordedEvents(t, r)
			found := false
			for _, e := range recorded {
				if strings.Contains(e, " Initialized ") {
					found = true
					if !strings.Contains(e, tt.wantSays) || strings.Contains(e, tt.wantNot) {
						t.Errorf("event %q, want it to say %q and not %q", e, tt.wantSays, tt.wantNot)
					}
				}
			}
			if !found {
				t.Errorf("no Initialized event in %q", recorded)
			}
		})
	}
}

func TestASeededPasswordNamesItsSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		seeded bool
	}{
		{name: "seeded", seeded: true},
		{name: "generated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			objs := []client.Object{bucketRepo(nil)}
			if tt.seeded {
				objs = []client.Object{bucketRepo(usePasswordSeed), seedSecretFixture()}
			}
			r, _ := newHarness(t, f, objs...)
			reconcileN(t, r, 3)
			want := "Using the restic password from Secret " + seedSecret + " key " + KeyResticPassword
			n := 0
			for _, e := range recordedEvents(t, r) {
				if strings.Contains(e, want) {
					n++
				}
			}
			if tt.seeded && n != 1 {
				t.Errorf("%d events say %q, want 1", n, want)
			}
			if !tt.seeded && n != 0 {
				t.Errorf("a generated password was said to come from %s", seedSecret)
			}
		})
	}
}

func TestTheInitOutcomeOutlivesAPassFromAStaleCopy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		existing   bool
		message    string
		wantReason string
	}{
		{name: "an adopted repository", existing: true, message: initFound + "\n", wantReason: reasonFoundExisting},
		{name: caseEmptyPath, message: initCreated + "\n", wantReason: reasonCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			bucket := f.AddBucket(sharedBucket)
			if tt.existing {
				f.PutFile(bucket.BucketID, configPath(myappPath), 1)
			}
			var snapshot atomic.Pointer[b2backupv1.Repository]
			r, c := newHarnessWithInterceptor(t, f, interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
					opts ...client.GetOption) error {
					if repo, ok := obj.(*b2backupv1.Repository); ok && snapshot.Load() != nil {
						snapshot.Load().DeepCopyInto(repo)
						return nil
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}, prefixRepo(usePasswordSeed), seedSecretFixture())
			reconcileN(t, r, 1)
			stale := getRepo(t, c)
			finishInitPod(t, c, tt.message, true)
			reconcileN(t, r, 1)

			snapshot.Store(stale)
			reconcileN(t, r, 1)
			snapshot.Store(nil)
			reconcileN(t, r, 1)

			var live b2backupv1.Repository
			if err := r.APIReader.Get(context.Background(), client.ObjectKeyFromObject(stale), &live); err != nil {
				t.Fatal(err)
			}
			cond := apimeta.FindStatusCondition(live.Status.Conditions, b2backupv1.RepositoryConditionInitialized)
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tt.wantReason {
				t.Errorf("Initialized = %+v, want True/%s", cond, tt.wantReason)
			}
		})
	}
}
