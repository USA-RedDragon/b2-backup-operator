package tui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/types"
)

// doctorScreen runs b2bk doctor's checks, failing ones first, with a next step
// under each that has one.
type doctorScreen struct {
	key     types.NamespacedName
	gen     int
	checks  []Check
	err     error
	loading bool
	sel     int
	top0    int
}

func newDoctorScreen(k types.NamespacedName) *doctorScreen { return &doctorScreen{key: k} }

type doctorMsg struct {
	screen *doctorScreen
	gen    int
	checks []Check
	err    error
}

func (msg doctorMsg) route() {
	d := msg.screen
	if msg.gen != d.gen {
		return
	}
	d.loading, d.err = false, msg.err
	d.checks = slices.Clone(msg.checks)
	slices.SortStableFunc(d.checks, func(a, b Check) int { return cmp.Compare(rank(b.Level), rank(a.Level)) })
}

func rank(l Level) int {
	switch l {
	case LevelFail:
		return 3
	case LevelWarn:
		return 2
	case LevelInfo:
		return 1
	}
	return 0
}

func (d *doctorScreen) run(m *Model) tea.Cmd {
	d.gen++
	d.loading = true
	gen, b, k := d.gen, m.backend, d.key
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(m.ctx)
		defer cancel()
		checks, err := b.Doctor(ctx, k.Namespace, k.Name)
		return doctorMsg{screen: d, gen: gen, checks: checks, err: err}
	}
}

func (d *doctorScreen) typing(*Model) bool { return false }

func (d *doctorScreen) crumbs(*Model) []string {
	return []string{d.key.Namespace, d.key.Name, wordDoctor}
}

func (d *doctorScreen) headerRight(*Model) string {
	if d.loading {
		return styleDim.Render("checking" + symEllipsis)
	}
	var fail, warn int
	for _, c := range d.checks {
		switch c.Level {
		case LevelFail:
			fail++
		case LevelWarn:
			warn++
		}
	}
	if fail+warn == 0 {
		return styleOK.Render("all checks pass")
	}
	return fmt.Sprintf("%s · %s", styleFail.Render(fmt.Sprintf("%d failing", fail)),
		styleWarn.Render(plural(warn, "warning")))
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (d *doctorScreen) top(*Model, int) []string { return nil }

func (d *doctorScreen) strip(*Model, int) string { return "" }

func symbolFor(l Level) string {
	switch l {
	case LevelFail:
		return styleFail.Render(symFail)
	case LevelWarn:
		return styleWarn.Render(symWarn)
	case LevelInfo:
		return styleDim.Render(symInfo)
	}
	return styleOK.Render(symSucceeded)
}

func stepText(s Step, l Level) string {
	switch s {
	case StepLogs:
		return styleKey.Render("l") + " view log"
	case StepBackup:
		if l != LevelFail {
			return styleKey.Render("b") + " back up now"
		}
		return styleKey.Render("b") + " run again"
	case StepResume:
		return styleKey.Render("p") + " resume"
	}
	return string(s)
}

// lines lays the checks out, with the line each check starts on.
func (d *doctorScreen) lines(width int) ([]string, []int) {
	var out []string
	var starts []int
	for i, c := range d.checks {
		starts = append(starts, len(out))
		cursor := " "
		if i == d.sel {
			cursor = styleAccent.Render(symCursor)
		}
		summary := c.Summary
		if i == d.sel {
			summary = styleSelected.Render(summary)
		}
		out = append(out, fmt.Sprintf(" %s%s %s", cursor, symbolFor(c.Level), summary))
		if c.Level == LevelOK {
			continue
		}
		for _, line := range c.Detail {
			out = append(out, "      "+styleDim.Render(trunc(line, width-8)))
		}
		if len(c.Next) > 0 {
			var steps []string
			for _, s := range c.Next {
				steps = append(steps, stepText(s, c.Level))
			}
			out = append(out, "      → "+strings.Join(steps, "   "))
		}
	}
	return out, starts
}

func (d *doctorScreen) body(_ *Model, width, height int) []string {
	switch {
	case d.err != nil:
		return append([]string{""}, wrapped(errText(d.err), width, "  ", styleFail)...)
	case d.loading && len(d.checks) == 0:
		return []string{"", styleDim.Render("  running the checks of b2bk doctor" + symEllipsis)}
	}
	out, starts := d.lines(width)
	if len(starts) == 0 {
		return nil
	}
	d.sel = max(0, min(d.sel, len(starts)-1))
	start := starts[d.sel]
	if start < d.top0 {
		d.top0 = start
	}
	end := len(out)
	if d.sel+1 < len(starts) {
		end = starts[d.sel+1]
	}
	if end > d.top0+height {
		d.top0 = end - height
	}
	d.top0 = max(0, min(d.top0, len(out)-height))
	return out[d.top0:min(len(out), d.top0+height)]
}

func (d *doctorScreen) bindings(*Model) []binding {
	return []binding{
		{keys: []string{keyEnter}, disp: "⏎", label: "act on selected"},
		{disp: "R", label: "re-run checks"},
		{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
		{keys: []string{keyUp}, disp: "↑↓", label: "choose a check", helpOnly: true},
		{disp: "l", label: "logs of the last run", act: actLogs, helpOnly: true},
		{disp: "b", label: wordBackupNow, act: actBackup, helpOnly: true},
		{disp: "p", label: "pause or resume", act: actSuspend, helpOnly: true},
	}
}

func (d *doctorScreen) handleKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	r := m.rowFor(d.key)
	switch msg.String() {
	case keyUp, "k":
		d.sel = max(0, d.sel-1)
	case keyDown, "j":
		d.sel = min(len(d.checks)-1, d.sel+1)
	case "R":
		return d.run(m), true
	case keyEnter:
		if r == nil || d.sel >= len(d.checks) || len(d.checks[d.sel].Next) == 0 {
			return nil, true
		}
		return d.step(m, r, d.checks[d.sel].Next[0]), true
	case "l":
		if r != nil {
			return m.openLogs(r, ""), true
		}
	case "b":
		if r != nil {
			return m.backupNow(r), true
		}
	case "p":
		if r != nil {
			return m.togglePause(r), true
		}
	default:
		return nil, false
	}
	return nil, true
}

func (d *doctorScreen) step(m *Model, r *row, s Step) tea.Cmd {
	switch s {
	case StepLogs:
		return m.openLogs(r, "")
	case StepBackup:
		return m.backupNow(r)
	case StepResume:
		return m.togglePause(r)
	}
	return nil
}

func (d *doctorScreen) click(m *Model, _, y int) tea.Cmd {
	_, starts := d.lines(m.width)
	line := d.top0 + y
	for i, start := range slices.Backward(starts) {
		if line >= start {
			d.sel = i
			return nil
		}
	}
	return nil
}

func (d *doctorScreen) scroll(_ *Model, delta int) {
	d.sel = max(0, min(len(d.checks)-1, d.sel+delta))
}
