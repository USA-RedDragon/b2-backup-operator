package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/record"
)

// CoreEvents writes the controllers' Events through the core/v1 API.
//
// The events.k8s.io/v1 recorder the manager hands out sets only eventTime and
// series, and that API rejects the deprecated firstTimestamp, lastTimestamp
// and count. kubectl get events reads core/v1, where those come out empty, so
// --sort-by=.lastTimestamp puts every operator Event first as <nil>. A core/v1
// Event carries all three.
type CoreEvents struct {
	broadcaster record.EventBroadcaster
	scheme      *runtime.Scheme
}

// NewCoreEvents starts sending Events to the API server through cs.
func NewCoreEvents(ctx context.Context, cs kubernetes.Interface, scheme *runtime.Scheme) *CoreEvents {
	b := record.NewBroadcaster(record.WithContext(ctx))
	b.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	return &CoreEvents{broadcaster: b, scheme: scheme}
}

// Recorder returns a recorder that reports Events as component.
func (e *CoreEvents) Recorder(component string) events.EventRecorder {
	return coreRecorder{rec: e.broadcaster.NewRecorder(e.scheme, corev1.EventSource{Component: component})}
}

// Shutdown flushes the queued Events and stops sending.
func (e *CoreEvents) Shutdown() {
	e.broadcaster.Shutdown()
}

// coreRecorder adapts a core/v1 recorder to the events.k8s.io signature the
// controllers call. A core/v1 Event has no related object, and the record
// package does not set its action, so both are dropped.
type coreRecorder struct {
	rec record.EventRecorder
}

func (c coreRecorder) Eventf(
	regarding, _ runtime.Object, eventtype, reason, _, note string, args ...any,
) {
	c.rec.Eventf(regarding, eventtype, reason, note, args...)
}
