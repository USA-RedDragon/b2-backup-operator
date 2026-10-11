package runner_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	claimNS      = "prod"
	claimJob     = "web-files-b2bk-restore-abcde"
	claimPod     = claimJob + "-0"
	freshClaim   = "inspect"
	unboundMsg   = "0/5 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/5 nodes are available"
	untaintedMsg = "0/5 nodes are available: 5 node(s) had untolerated taint {node.kubernetes.io/unreachable: }"
)

type claimCase struct {
	name      string
	phase     corev1.PersistentVolumeClaimPhase
	message   string
	missing   bool
	bindLater bool
	wantErr   error
	wantText  string
}

func TestWaitForPodWaitsForItsClaimsToBind(t *testing.T) {
	t.Parallel()
	cases := []claimCase{
		{name: "a claim still provisioning", phase: corev1.ClaimPending, message: unboundMsg, bindLater: true},
		{name: "a bound claim the scheduler has not retried yet", phase: corev1.ClaimBound, message: unboundMsg,
			bindLater: true},
		{name: "a bound claim and an unrelated reason", phase: corev1.ClaimBound, message: untaintedMsg,
			wantErr: runner.ErrFailed, wantText: "untolerated taint"},
		{name: "an unbound claim and an unrelated reason", phase: corev1.ClaimPending, message: untaintedMsg,
			wantErr: runner.ErrFailed, wantText: "untolerated taint"},
		{name: "a claim that is gone", missing: true, message: unboundMsg,
			wantErr: runner.ErrMissingDependency, wantText: freshClaim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, job := unschedulableJob(t, tc)
			if tc.bindLater {
				go bindAndStart(t, r.Client)
			}
			pod, err := r.WaitForPod(context.Background(), job)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("WaitForPod gave up on a pod whose claim was binding: %v", err)
				}
				if pod.Name != claimPod {
					t.Errorf("pod = %q, want %q", pod.Name, claimPod)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err %q does not mention %q", err, tc.wantText)
			}
		})
	}
}

func unschedulableJob(t *testing.T, tc claimCase) (*runner.Runner, *batchv1.Job) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	job := &batchv1.Job{Namespace: claimNS, Name: claimJob}
	pod := &corev1.Pod{
		Namespace: claimNS, Name: claimPod,
		Labels: map[string]string{batchv1.JobNameLabel: claimJob},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name:                  "restore-target",
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: freshClaim},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable, Message: tc.message,
			}},
		},
	}
	objs := []client.Object{job, pod}
	if !tc.missing {
		objs = append(objs, &corev1.PersistentVolumeClaim{
			Namespace: claimNS, Name: freshClaim,
			Status: corev1.PersistentVolumeClaimStatus{Phase: tc.phase},
		})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&corev1.Pod{}, &corev1.PersistentVolumeClaim{}).Build()
	return &runner.Runner{Client: c, PodStartTimeout: 20 * time.Second}, job
}

func bindAndStart(t *testing.T, c client.Client) {
	t.Helper()
	time.Sleep(1500 * time.Millisecond)
	ctx := context.Background()
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: claimNS, Name: freshClaim}, &pvc); err != nil {
		t.Error(err)
		return
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, &pvc); err != nil {
		t.Error(err)
		return
	}
	time.Sleep(time.Second)
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: claimNS, Name: claimPod}, &pod); err != nil {
		t.Error(err)
		return
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
	if err := c.Status().Update(ctx, &pod); err != nil {
		t.Error(err)
	}
}
