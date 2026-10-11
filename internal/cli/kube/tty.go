package kube

import (
	"context"
	"time"

	"golang.org/x/term"
	"k8s.io/client-go/tools/remotecommand"
)

const resizePoll = 250 * time.Millisecond

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd uintptr) bool { return term.IsTerminal(int(fd)) }

// NewSizeQueue returns a queue that reports terminal resizes on fd until ctx ends.
func NewSizeQueue(ctx context.Context, fd uintptr) remotecommand.TerminalSizeQueue {
	return &sizeQueue{
		ctx: ctx,
		get: func() (int, int, error) { return term.GetSize(int(fd)) },
	}
}

type sizeQueue struct {
	ctx  context.Context
	get  func() (int, int, error)
	last remotecommand.TerminalSize
}

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	for {
		if q.ctx.Err() != nil {
			return nil
		}

		w, h, err := q.get()
		if err != nil {
			return nil
		}
		size := remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
		if size != q.last {
			q.last = size
			return &size
		}

		select {
		case <-q.ctx.Done():
			return nil
		case <-time.After(resizePoll):
		}
	}
}

// WithRawTerminal puts fd into raw mode for the duration of fn.
func WithRawTerminal(fd uintptr, fn func() error) error {
	state, err := term.MakeRaw(int(fd))
	if err != nil {
		return err
	}
	defer func() { _ = term.Restore(int(fd), state) }()
	return fn()
}
