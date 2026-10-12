package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/apimachinery/pkg/types"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// logView scrolls, wraps and searches the lines of a stream.
type logView struct {
	follow    bool
	offset    int
	wrap      bool
	search    field
	searching bool
	lastH     int
}

func (v *logView) render(sv streamView, loc *time.Location, width, height int) []string {
	v.lastH = height
	var rendered []string
	needle := v.search.value
	for _, l := range sv.lines {
		text := l.text
		prefix := ""
		if !l.at.IsZero() {
			prefix = styleDim.Render(l.at.In(loc).Format("15:04:05")) + " "
		}
		if needle != "" && strings.Contains(text, needle) {
			text = strings.ReplaceAll(text, needle, styleBadge.Render(needle))
		}
		line := "  " + prefix + text
		if v.wrap {
			for part := range strings.SplitSeq(ansi.Hardwrap(line, max(10, width-1), true), "\n") {
				rendered = append(rendered, part)
			}
			continue
		}
		rendered = append(rendered, line)
	}
	if sv.done && sv.err != nil {
		rendered = append(rendered, wrapped(errText(sv.err), width, "  ", styleFail)...)
	}
	if !sv.done && len(sv.lines) == 0 {
		rendered = append(rendered, styleDim.Render("  waiting for output"+symEllipsis))
	}
	if !sv.done && v.follow {
		rendered = append(rendered, "  "+symBar)
	}
	if v.follow {
		v.offset = len(rendered) - height
	}
	v.offset = max(0, min(v.offset, len(rendered)-height))
	end := min(len(rendered), v.offset+height)
	return rendered[v.offset:end]
}

func (v *logView) key(msg tea.KeyPressMsg) bool {
	k := msg.String()
	if v.searching {
		switch k {
		case keyEnter, keyEsc:
			v.searching, v.search.focused = false, false
			if k == keyEsc {
				v.search.value = ""
			}
		default:
			v.search.edit(msg)
		}
		return true
	}
	page := max(1, v.lastH-1)
	switch k {
	case "f":
		v.follow = !v.follow
	case "w":
		v.wrap = !v.wrap
	case "/":
		v.searching, v.search.focused, v.follow = true, true, false
	case "g", "home":
		v.follow, v.offset = false, 0
	case "G", "end":
		v.follow = true
	case keyUp, "k":
		v.follow, v.offset = false, v.offset-1
	case keyDown, "j":
		v.offset++
	case keyPgUp:
		v.follow, v.offset = false, v.offset-page
	case keyPgDown, keySpace:
		v.offset += page
	default:
		return false
	}
	return true
}

func (v *logView) scroll(delta int) {
	if delta < 0 {
		v.follow = false
	}
	v.offset += delta
}

// logsScreen follows a running backup's Job or shows a finished one.
type logsScreen struct {
	key  types.NamespacedName
	job  string
	view logView
}

func newLogsScreen(m *Model, k types.NamespacedName, job string) *logsScreen {
	s := &logsScreen{key: k, job: job}
	s.view.follow = true
	m.logStream(k.Namespace, job, s.running(m))
	return s
}

func (s *logsScreen) running(m *Model) bool {
	r := m.rowFor(s.key)
	if r == nil {
		return false
	}
	for i := range r.jobs {
		if r.jobs[i].Name == s.job {
			return !jobFinished(&r.jobs[i])
		}
	}
	return false
}

func (s *logsScreen) typing(*Model) bool { return s.view.searching }

func (s *logsScreen) crumbs(*Model) []string { return []string{s.key.Namespace, s.key.Name, "logs"} }

func (s *logsScreen) stream(m *Model) streamView {
	if st, ok := m.streams[s.key.Namespace+"/"+s.job]; ok {
		return st.snapshot()
	}
	return streamView{done: true}
}

func (s *logsScreen) headerRight(m *Model) string {
	state := s.result(m)
	follow := styleDim.Render("f to follow")
	if s.view.follow {
		follow = "following · " + styleDim.Render("f to stop")
	}
	return "job/" + s.job + "   " + state + "     " + follow
}

func (s *logsScreen) result(m *Model) string {
	r := m.rowFor(s.key)
	if r == nil {
		return ""
	}
	for i := range r.jobs {
		job := &r.jobs[i]
		if job.Name != s.job {
			continue
		}
		switch {
		case job.Status.Succeeded > 0:
			return styleOK.Render(symSucceeded + " succeeded")
		case job.Status.Failed > 0:
			text := symFail + " failed"
			if exit := jobExit(job, r.pods[job.Name]); exit != "" {
				text += "  " + exit
			}
			return styleFail.Render(text)
		}
		return styleWarn.Render(symRunning + " running " + clock(m.now().Sub(jobStart(job))))
	}
	for _, h := range r.sb.Status.History {
		if h.JobName == s.job {
			switch h.Result {
			case b2backupv1.BackupRunSucceeded:
				return styleOK.Render(symSucceeded + " succeeded")
			case b2backupv1.BackupRunFailed:
				return styleFail.Render(symFail + " failed")
			}
		}
	}
	return ""
}

func (s *logsScreen) top(m *Model, width int) []string {
	sv := s.stream(m)
	if sv.progress == nil {
		return nil
	}
	return []string{progressBarLine(sv.label, sv.progress, width)}
}

// progressBarLine draws restic's progress as one bar line.
func progressBarLine(label string, p *progress, width int) string {
	right := []string{}
	if p.hasPercent {
		right = append(right, fmt.Sprintf("%3.0f%%", p.percent))
	}
	right = append(right, p.done+" / "+p.total)
	if p.eta > 0 {
		right = append(right, "ETA "+clock(p.eta))
	}
	tail := strings.Join(right, "   ")
	head := "  " + label
	barWidth := width - lipgloss.Width(head) - lipgloss.Width(tail) - 6
	if !p.hasPercent || barWidth < 8 {
		return spread(head, tail+"  ", width)
	}
	return head + "  " + bar(p.percent/100, barWidth) + "  " + tail
}

func (s *logsScreen) body(m *Model, width, height int) []string {
	loc := time.UTC
	if r := m.rowFor(s.key); r != nil {
		loc = r.location()
	}
	return s.view.render(s.stream(m), loc, width, height)
}

func (s *logsScreen) strip(*Model, int) string {
	if s.view.searching {
		return " " + styleKey.Render("/") + " " + s.view.search.view(30) + "  " + styleDim.Render("⏎ keep  esc clear")
	}
	return ""
}

func (s *logsScreen) bindings(*Model) []binding {
	return []binding{
		{disp: "f", label: wordFollow},
		{disp: "/", label: "search"},
		{disp: "w", label: wordWrap},
		{disp: "g/G", keys: []string{"g"}, label: "top/end"},
		{disp: "[ ]", keys: []string{"["}, label: "older/newer run"},
		{disp: "x", label: "cancel run", act: actCancel},
		{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
		{disp: "PgUp PgDn", label: "page", helpOnly: true},
	}
}

func (s *logsScreen) handleKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if s.view.key(msg) {
		return nil, true
	}
	r := m.rowFor(s.key)
	if r == nil {
		return nil, false
	}
	switch msg.String() {
	case "x":
		return m.cancelRun(r), true
	case "[", "]":
		runs := runsOf(m, r)
		for i := range runs {
			if runs[i].job != s.job {
				continue
			}
			j := i + 1
			if msg.String() == "]" {
				j = i - 1
			}
			if j >= 0 && j < len(runs) {
				s.job = runs[j].job
				s.view = logView{follow: true}
				m.logStream(s.key.Namespace, s.job, s.running(m))
			}
			break
		}
		return nil, true
	}
	return nil, false
}

func (s *logsScreen) click(*Model, int, int) tea.Cmd { return nil }

func (s *logsScreen) scroll(_ *Model, delta int) { s.view.scroll(delta) }
