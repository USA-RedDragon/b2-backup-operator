package tui

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

type podState int

const (
	podStarting podState = iota
	podReady
	podFailed
)

// session is the one restic pod the TUI keeps per backup while it browses,
// with what it has read through it.
type session struct {
	mu       sync.Mutex
	key      types.NamespacedName
	state    podState
	pod      Pod
	err      error
	started  time.Time
	progress []string

	snapshots     []Snapshot
	snapshotsErr  error
	snapshotsDone bool
	loadingSnaps  bool

	files       map[string][]Node
	filesErr    map[string]error
	filesLoaded map[string]bool
}

// sessions owns every session pod, and removes them all on Close.
type sessions struct {
	mu      sync.Mutex
	ctx     context.Context
	backend Backend
	hub     *hub
	now     func() time.Time
	all     map[types.NamespacedName]*session
	wg      sync.WaitGroup
	closed  bool
}

func newSessions(ctx context.Context, b Backend, h *hub, now func() time.Time) *sessions {
	return &sessions{ctx: ctx, backend: b, hub: h, now: now, all: map[types.NamespacedName]*session{}}
}

type progressWriter struct {
	s *session
	h *hub
}

func (w progressWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			w.s.progress = append(w.s.progress, line)
		}
	}
	w.s.mu.Unlock()
	w.h.notify()
	return len(p), nil
}

// open returns the backup's session, starting its pod when there is none or
// the last one failed.
func (ss *sessions) open(key types.NamespacedName) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if s, ok := ss.all[key]; ok && (s.view().state != podFailed || ss.closed) {
		return
	}
	s := &session{
		key: key, started: ss.now(),
		files: map[string][]Node{}, filesErr: map[string]error{}, filesLoaded: map[string]bool{},
	}
	ss.all[key] = s
	if ss.closed {
		s.state, s.err = podFailed, context.Canceled
		return
	}
	ss.wg.Go(func() {
		pod, err := ss.backend.OpenPod(ss.ctx, key.Namespace, key.Name, progressWriter{s: s, h: ss.hub})
		s.mu.Lock()
		if err != nil {
			s.state, s.err = podFailed, err
		} else {
			s.state, s.pod = podReady, pod
		}
		s.mu.Unlock()
		ss.mu.Lock()
		closed := ss.closed
		ss.mu.Unlock()
		if closed && pod != nil {
			pod.Close()
		}
		ss.hub.notify()
		if err == nil {
			ss.loadSnapshots(s)
		}
	})
}

func (ss *sessions) get(key types.NamespacedName) *session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.all[key]
}

func (ss *sessions) loadSnapshots(s *session) {
	s.mu.Lock()
	if s.loadingSnaps || s.pod == nil {
		s.mu.Unlock()
		return
	}
	s.loadingSnaps, s.snapshotsDone = true, false
	pod := s.pod
	s.mu.Unlock()
	ss.hub.notify()
	snaps, err := ss.backend.Snapshots(ss.ctx, pod, s.key.Namespace, s.key.Name)
	slices.SortStableFunc(snaps, func(a, b Snapshot) int { return b.Time.Compare(a.Time) })
	s.mu.Lock()
	s.snapshots, s.snapshotsErr, s.snapshotsDone, s.loadingSnaps = snaps, err, true, false
	s.mu.Unlock()
	ss.hub.notify()
}

// reloadSnapshots lists the snapshots again in the background.
func (ss *sessions) reloadSnapshots(s *session) {
	ss.wg.Go(func() { ss.loadSnapshots(s) })
}

// loadFiles lists a snapshot's files in the background, once per session.
func (ss *sessions) loadFiles(s *session, id string) {
	s.mu.Lock()
	if s.filesLoaded[id] || s.pod == nil {
		s.mu.Unlock()
		return
	}
	s.filesLoaded[id] = true
	pod := s.pod
	s.mu.Unlock()
	ss.wg.Go(func() {
		nodes, err := ss.backend.Files(ss.ctx, pod, id)
		s.mu.Lock()
		if err != nil {
			delete(s.filesLoaded, id)
			s.filesErr[id] = err
		} else {
			s.files[id] = nodes
			delete(s.filesErr, id)
		}
		s.mu.Unlock()
		ss.hub.notify()
	})
}

type sessionView struct {
	state         podState
	podName       string
	err           error
	started       time.Time
	progress      []string
	snapshots     []Snapshot
	snapshotsErr  error
	snapshotsDone bool
	pod           Pod
}

func (s *session) view() sessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := sessionView{
		state: s.state, err: s.err, started: s.started, progress: s.progress,
		snapshots: s.snapshots, snapshotsErr: s.snapshotsErr, snapshotsDone: s.snapshotsDone, pod: s.pod,
	}
	if s.pod != nil {
		v.podName = s.pod.Name()
	}
	return v
}

func (s *session) filesOf(id string) ([]Node, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes, ok := s.files[id]
	return nodes, ok, s.filesErr[id]
}

// closeAll removes every session pod and waits until each is gone.
func (ss *sessions) closeAll() {
	ss.mu.Lock()
	ss.closed = true
	all := make([]*session, 0, len(ss.all))
	for _, s := range ss.all {
		all = append(all, s)
	}
	ss.mu.Unlock()
	for _, s := range all {
		s.mu.Lock()
		pod := s.pod
		s.mu.Unlock()
		if pod != nil {
			pod.Close()
		}
	}
	ss.wg.Wait()
}

var _ io.Writer = progressWriter{}
