package kube

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/tools/remotecommand"
)

func TestSizeQueueStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &sizeQueue{ctx: ctx, get: func() (int, int, error) { return 80, 24, nil }}

	if q.Next() == nil {
		t.Fatal("the starting size should be reported")
	}

	next := make(chan *remotecommand.TerminalSize, 1)
	go func() { next <- q.Next() }()

	select {
	case <-next:
		t.Fatal("Next returned while the size was unchanged and the context live")
	case <-time.After(2 * resizePoll):
	}

	cancel()
	select {
	case got := <-next:
		if got != nil {
			t.Errorf("Next = %+v, want nil so client-go's resize goroutine can exit", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next never returned; the resize goroutine outlives the exec")
	}
}
