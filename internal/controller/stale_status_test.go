package controller

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const (
	reasonCredentialsUnavailable = "CredentialsUnavailable"
	reasonSuspended              = "Suspended"
	reasonReconcileFailed        = "ReconcileFailed"
)

func TestAStalePassKeepsTheConditionsItDidNotChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stale     func(*b2backupv1.Repository)
		before    func(*testing.T, client.Client)
		wantReady string
	}{
		{name: "a pass that cannot read the B2 credentials", wantReady: reasonCredentialsUnavailable,
			before: func(t *testing.T, c client.Client) {
				t.Helper()
				secret := &corev1.Secret{Name: credsName, Namespace: credsNS}
				if err := c.Delete(context.Background(), secret); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a suspended pass", wantReady: reasonSuspended,
			stale: func(r *b2backupv1.Repository) { r.Spec.Suspend = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeB2(t)
			bucket := f.AddBucket(sharedBucket)
			f.PutFile(bucket.BucketID, configPath(myappPath), 1)
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
			if tt.stale != nil {
				tt.stale(stale)
			}
			finishInitPod(t, c, initFound+"\n", true)
			reconcileN(t, r, 1)

			if tt.before != nil {
				tt.before(t, c)
			}
			snapshot.Store(stale)
			_, _ = r.Reconcile(context.Background(), ctrl.Request{Namespace: testNS, Name: resticName})

			var live b2backupv1.Repository
			if err := r.APIReader.Get(context.Background(), client.ObjectKeyFromObject(stale), &live); err != nil {
				t.Fatal(err)
			}
			init := apimeta.FindStatusCondition(live.Status.Conditions, b2backupv1.RepositoryConditionInitialized)
			if init == nil || init.Status != metav1.ConditionTrue || init.Reason != reasonFoundExisting {
				t.Errorf("Initialized = %+v, want True/%s", init, reasonFoundExisting)
			}
			owned := apimeta.FindStatusCondition(live.Status.Conditions, b2backupv1.RepositoryConditionOwned)
			if owned == nil || owned.Status != metav1.ConditionTrue {
				t.Errorf("Owned = %+v, want True", owned)
			}
			ready := apimeta.FindStatusCondition(live.Status.Conditions, b2backupv1.RepositoryConditionReady)
			if ready == nil || ready.Reason != tt.wantReady {
				t.Errorf("Ready = %+v, want reason %s", ready, tt.wantReady)
			}
		})
	}
}

func TestRebaseConditions(t *testing.T) {
	t.Parallel()
	cond := func(typ, reason string) metav1.Condition {
		return metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: reason}
	}
	tests := []struct {
		name     string
		base     []metav1.Condition
		modified []metav1.Condition
		live     []metav1.Condition
		want     []metav1.Condition
	}{
		{name: "an unchanged condition keeps the live one",
			base:     []metav1.Condition{cond("A", "Old"), cond("B", "Old")},
			modified: []metav1.Condition{cond("A", "Old"), cond("B", "Pass")},
			live:     []metav1.Condition{cond("A", "Live"), cond("B", "Live")},
			want:     []metav1.Condition{cond("A", "Live"), cond("B", "Pass")}},
		{name: "a condition only the live copy has stays",
			base:     []metav1.Condition{cond("A", "Old")},
			modified: []metav1.Condition{cond("A", "Pass")},
			live:     []metav1.Condition{cond("A", "Live"), cond("C", "Live")},
			want:     []metav1.Condition{cond("A", "Pass"), cond("C", "Live")}},
		{name: "a condition the pass added is added",
			modified: []metav1.Condition{cond("A", "Pass")},
			live:     []metav1.Condition{cond("C", "Live")},
			want:     []metav1.Condition{cond("C", "Live"), cond("A", "Pass")}},
		{name: "a condition the pass removed is removed",
			base: []metav1.Condition{cond("A", "Old")},
			live: []metav1.Condition{cond("A", "Live"), cond("C", "Live")},
			want: []metav1.Condition{cond("C", "Live")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			live := slices.Clone(tt.live)
			got := rebaseConditions(tt.base, tt.modified, live)
			if !slices.Equal(got, tt.want) {
				t.Errorf("rebaseConditions = %+v, want %+v", got, tt.want)
			}
			if !slices.Equal(live, tt.live) {
				t.Errorf("rebaseConditions changed the live slice to %+v", live)
			}
		})
	}
}

func TestAStaleScheduledBackupPassKeepsTheLiveHistory(t *testing.T) {
	t.Parallel()
	at := func(h int) *metav1.Time {
		return &metav1.Time{Time: time.Date(2026, 10, 1, h, 0, 0, 0, time.UTC)}
	}
	run := func(name string, h int) b2backupv1.BackupRun {
		return b2backupv1.BackupRun{JobName: name, StartTime: at(h), CompletionTime: at(h),
			Result: b2backupv1.BackupRunSucceeded}
	}
	other := metav1.Condition{Type: "Other", Status: metav1.ConditionTrue, Reason: "Live",
		LastTransitionTime: *at(3)}
	ready := metav1.Condition{Type: b2backupv1.ScheduledBackupConditionReady, Status: metav1.ConditionTrue,
		Reason: "Ready", LastTransitionTime: *at(1)}
	tests := []struct {
		name        string
		passHistory []b2backupv1.BackupRun
		passLatest  int
		wantJobs    []string
		wantLatest  int
	}{
		{name: "a pass that changed only a condition", passLatest: 1,
			wantJobs: []string{"run-b", "run-a"}, wantLatest: 2},
		{name: "a pass that saw a new run", passHistory: []b2backupv1.BackupRun{run("run-c", 4), run("run-a", 1)},
			passLatest: 4, wantJobs: []string{"run-c", "run-b", "run-a"}, wantLatest: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sb := scheduledBackup()
			sb.Status = b2backupv1.ScheduledBackupStatus{
				Conditions:         []metav1.Condition{ready, other},
				History:            []b2backupv1.BackupRun{run("run-b", 2), run("run-a", 1)},
				LastSuccessfulTime: at(2),
			}
			r, c := backupHarness(t, sb)

			base := scheduledBackup()
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(base), base); err != nil {
				t.Fatal(err)
			}
			base.Status = b2backupv1.ScheduledBackupStatus{
				Conditions: []metav1.Condition{ready},
				History:    []b2backupv1.BackupRun{run("run-a", 1)},
			}
			modified := base.DeepCopy()
			modified.Status.LastSuccessfulTime = at(tt.passLatest)
			if tt.passHistory != nil {
				modified.Status.History = tt.passHistory
			}
			r.setCondition(modified, metav1.Condition{Type: b2backupv1.ScheduledBackupConditionReady,
				Status: metav1.ConditionFalse, Reason: reasonReconcileFailed})
			if err := r.patchStatus(context.Background(), modified, base); err != nil {
				t.Fatal(err)
			}

			var live b2backupv1.ScheduledBackup
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(sb), &live); err != nil {
				t.Fatal(err)
			}
			jobs := make([]string, 0, len(live.Status.History))
			for _, run := range live.Status.History {
				jobs = append(jobs, run.JobName)
			}
			if !slices.Equal(jobs, tt.wantJobs) {
				t.Errorf("history = %v, want %v", jobs, tt.wantJobs)
			}
			if got := apimeta.FindStatusCondition(live.Status.Conditions, "Other"); got == nil || got.Reason != "Live" {
				t.Errorf("Other = %+v, want the live condition", got)
			}
			if got := apimeta.FindStatusCondition(live.Status.Conditions,
				b2backupv1.ScheduledBackupConditionReady); got == nil || got.Reason != reasonReconcileFailed {
				t.Errorf("Ready = %+v, want the pass's ReconcileFailed", got)
			}
			if want := at(tt.wantLatest); !live.Status.LastSuccessfulTime.Equal(want) {
				t.Errorf("lastSuccessfulTime = %v, want %v", live.Status.LastSuccessfulTime, want)
			}
		})
	}
}
