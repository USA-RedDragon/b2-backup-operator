package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func TestAReadErrorBeforeTheCronJobIsSyncedSuspendsIt(t *testing.T) {
	t.Parallel()
	errAPI := errors.New("apiserver unavailable")
	tests := []struct {
		name  string
		funcs interceptor.Funcs
	}{
		{"the Repository cannot be read", interceptor.Funcs{Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if _, ok := obj.(*b2backupv1.Repository); ok {
				return errAPI
			}
			return cl.Get(ctx, key, obj, opts...)
		}}},
		{"the other backups cannot be listed", interceptor.Funcs{List: func(
			ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption,
		) error {
			if _, ok := list.(*b2backupv1.ScheduledBackupList); ok {
				return errAPI
			}
			return cl.List(ctx, list, opts...)
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r, c := backupHarness(t, initializedRepo(), scheduledBackup())
			reconcileBackup(t, r)
			r.Client = interceptor.NewClient(c.(client.WithWatch), tt.funcs)
			if _, err := r.Reconcile(ctx, ctrl.Request{Namespace: testNS, Name: resticName}); !errors.Is(err, errAPI) {
				t.Fatalf("err = %v, want the read error", err)
			}
			var cj batchv1.CronJob
			if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: cronJobName}, &cj); err != nil {
				t.Fatal(err)
			}
			if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
				t.Error("the CronJob kept firing while its spec could not be checked")
			}
		})
	}
}
