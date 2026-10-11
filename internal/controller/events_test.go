package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

func TestEventsCarryTheTimestampsKubectlSortsBy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		eventtype string
		reason    string
		note      string
	}{
		{name: "a normal event", eventtype: corev1.EventTypeNormal, reason: "BucketCreated", note: "Created bucket %s"},
		{name: "a warning", eventtype: corev1.EventTypeWarning, reason: "InitFailed", note: "Init job %s failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs := fake.NewClientset()
			got := emitOne(t, cs, tc.eventtype, tc.reason, tc.note)
			checkSortable(t, got, tc.eventtype, tc.reason)
		})
	}
}

func emitOne(t *testing.T, cs kubernetes.Interface, eventtype, reason, note string) corev1.Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := b2backupv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ev := controller.NewCoreEvents(ctx, cs, scheme)
	t.Cleanup(ev.Shutdown)
	repo := &b2backupv1.Repository{
		APIVersion: b2backupv1.GroupVersion.String(), Kind: "Repository",
		Namespace: lsNS, Name: lsRepo, UID: "uid-media",
	}
	ev.Recorder("repository-controller").Eventf(repo, nil, eventtype, reason, "Act", note, "acme")

	var found corev1.Event
	err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			list, err := cs.CoreV1().Events(lsNS).List(ctx, metav1.ListOptions{})
			if err != nil || len(list.Items) == 0 {
				return false, err
			}
			found = list.Items[0]
			return true, nil
		})
	if err != nil {
		t.Fatalf("no core/v1 Event was written: %v", err)
	}
	return found
}

func checkSortable(t *testing.T, got corev1.Event, eventtype, reason string) {
	t.Helper()
	if got.LastTimestamp.IsZero() || got.FirstTimestamp.IsZero() {
		t.Errorf("firstTimestamp = %v, lastTimestamp = %v; kubectl --sort-by=.lastTimestamp needs both",
			got.FirstTimestamp, got.LastTimestamp)
	}
	if got.Count < 1 {
		t.Errorf("count = %d, want at least 1", got.Count)
	}
	if got.Type != eventtype || got.Reason != reason || !strings.Contains(got.Message, "acme") ||
		got.InvolvedObject.Kind != "Repository" || got.InvolvedObject.Name != lsRepo {
		t.Errorf("event = %s %s %q on %v", got.Type, got.Reason, got.Message, got.InvolvedObject)
	}
	if got.Source.Component != "repository-controller" {
		t.Errorf("source = %q, want the controller", got.Source.Component)
	}
}
