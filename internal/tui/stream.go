package tui

import (
	"bufio"
	"context"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// hub wakes the event loop when background work has changed what is shown,
// and counts the work still running, so quitting can wait for its cleanup.
type hub struct {
	ch chan struct{}
	wg sync.WaitGroup
}

func newHub() *hub { return &hub{ch: make(chan struct{}, 1)} }

func (h *hub) notify() {
	select {
	case h.ch <- struct{}{}:
	default:
	}
}

type eventMsg struct{}

func (h *hub) wait(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-h.ch:
			return eventMsg{}
		case <-ctx.Done():
			return nil
		}
	}
}

type logLine struct {
	at   time.Time
	text string
}

// progress is restic's latest status line.
type progress struct {
	percent    float64
	hasPercent bool
	done       string
	total      string
	eta        time.Duration
	elapsed    time.Duration
}

// stream collects the lines something writes, with restic's progress lines
// taken out and kept as the latest progress instead.
type stream struct {
	mu       sync.Mutex
	lines    []logLine
	progress *progress
	label    string
	done     bool
	err      error
	started  time.Time
	cancel   context.CancelFunc
}

const maxLineBytes = 1 << 20

// startStream runs produce in the background, collecting what it writes.
func startStream(ctx context.Context, h *hub, now time.Time, produce func(context.Context, io.Writer) error) *stream {
	ctx, cancel := context.WithCancel(ctx)
	s := &stream{cancel: cancel, started: now}
	r, w := io.Pipe()
	h.wg.Go(func() {
		err := produce(ctx, w)
		_ = w.CloseWithError(err)
	})
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), maxLineBytes)
		for sc.Scan() {
			s.add(sc.Text())
			h.notify()
		}
		err := sc.Err()
		_ = r.CloseWithError(err)
		s.mu.Lock()
		s.done, s.err = true, err
		if ctx.Err() != nil && s.err != nil {
			s.err = nil
		}
		s.mu.Unlock()
		cancel()
		h.notify()
	}()
	return s
}

func (s *stream) stop() { s.cancel() }

func (s *stream) add(raw string) {
	raw = strings.TrimRight(raw, "\r")
	if i := strings.LastIndexByte(raw, '\r'); i >= 0 {
		raw = raw[i+1:]
	}
	line := logLine{text: raw}
	if ts, rest, ok := strings.Cut(raw, " "); ok && len(ts) >= 20 && ts[4] == '-' {
		if at, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			line = logLine{at: at, text: rest}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := parseProgress(line.text); ok {
		s.progress = &p
		return
	}
	if label := commandLabel(line.text); label != "" {
		s.label = label
		s.progress = nil
	}
	s.lines = append(s.lines, line)
}

type streamView struct {
	lines    []logLine
	progress *progress
	label    string
	done     bool
	err      error
	started  time.Time
}

func (s *stream) snapshot() streamView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p *progress
	if s.progress != nil {
		cp := *s.progress
		p = &cp
	}
	return streamView{lines: s.lines, progress: p, label: s.label, done: s.done, err: s.err, started: s.started}
}

var progressLine = regexp.MustCompile(
	`^\[(?:(\d+):)?(\d+):(\d+)\]\s+(?:([\d.]+)%\s+)?\d+ files(?:/dirs)?,? ([\d.]+ [KMGTPE]?i?B)` +
		`(?:, total \d+ files(?:/dirs)? ([\d.]+ [KMGTPE]?i?B))?(?:, \d+ errors)?(?: ETA (?:(\d+):)?(\d+):(\d+))?`)

// parseProgress reads a restic status line, as restic prints one every few
// seconds when RESTIC_PROGRESS_FPS is set or a terminal is attached.
func parseProgress(text string) (progress, bool) {
	m := progressLine.FindStringSubmatch(text)
	if m == nil {
		return progress{}, false
	}
	p := progress{done: m[5], total: m[6]}
	if p.total == "" {
		p.total = "?"
	}
	p.elapsed = hms(m[1], m[2], m[3])
	if m[4] != "" {
		p.percent, _ = strconv.ParseFloat(m[4], 64)
		p.hasPercent = true
	}
	if m[8] != "" {
		p.eta = hms(m[7], m[8], m[9])
	}
	return p, true
}

func hms(h, m, s string) time.Duration {
	hours, _ := strconv.Atoi(h)
	minutes, _ := strconv.Atoi(m)
	seconds, _ := strconv.Atoi(s)
	return time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second
}

// commandLabel names what a traced restic command works on: "files /data"
// for "+ restic backup --tag=files /data".
func commandLabel(text string) string {
	rest, ok := strings.CutPrefix(text, "+ ")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 || fields[0] != "restic" {
		return ""
	}
	var tag string
	var args []string
	for _, f := range fields[2:] {
		switch {
		case strings.HasPrefix(f, "--tag="):
			tag = strings.TrimPrefix(f, "--tag=")
		case strings.HasPrefix(f, "-"):
		default:
			args = append(args, f)
		}
	}
	if fields[1] != "backup" && fields[1] != wordRestore {
		return ""
	}
	return strings.TrimSpace(tag + " " + strings.Join(args, " "))
}
