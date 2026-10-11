//go:build envtest

package controller_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

func TestEnvtestEventsSortByLastTimestamp(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		recorder func(ctx context.Context, t *testing.T) events.EventRecorder
		sortable bool
	}{
		{name: "the operator's core/v1 recorder", sortable: true,
			recorder: func(ctx context.Context, t *testing.T) events.EventRecorder {
				ev := controller.NewCoreEvents(ctx, cs, scheme)
				t.Cleanup(ev.Shutdown)
				return ev.Recorder("repository-controller")
			}},
		{name: "the manager's events.k8s.io recorder", sortable: false,
			recorder: func(ctx context.Context, t *testing.T) events.EventRecorder {
				b := events.NewBroadcaster(&events.EventSinkImpl{Interface: cs.EventsV1()})
				if err := b.StartRecordingToSinkWithContext(ctx); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(b.Shutdown)
				return b.NewRecorder(scheme, "repository-controller")
			}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			ns := &corev1.Namespace{Name: "events-" + string(rune('a'+i))}
			if _, err := cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			cm, err := cs.CoreV1().ConfigMaps(ns.Name).Create(ctx,
				&corev1.ConfigMap{Namespace: ns.Name, Name: "target"}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cm.APIVersion, cm.Kind = "v1", "ConfigMap"
			tc.recorder(ctx, t).Eventf(cm, nil, corev1.EventTypeNormal, "BucketCreated", "Create", "Created %s", "acme")

			var got corev1.Event
			err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
				func(ctx context.Context) (bool, error) {
					list, err := cs.CoreV1().Events(ns.Name).List(ctx, metav1.ListOptions{})
					if err != nil || len(list.Items) == 0 {
						return false, err
					}
					got = list.Items[0]
					return true, nil
				})
			if err != nil {
				t.Fatalf("no Event reached the API server: %v", err)
			}
			sortable := !got.LastTimestamp.IsZero() && !got.FirstTimestamp.IsZero()
			if sortable != tc.sortable {
				t.Errorf("firstTimestamp = %v, lastTimestamp = %v, eventTime = %v; sortable = %t, want %t",
					got.FirstTimestamp, got.LastTimestamp, got.EventTime, sortable, tc.sortable)
			}
		})
	}
}
