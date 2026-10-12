package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"k8s.io/apimachinery/pkg/types"

	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

// dashboard lists every backup in scope.
type dashboard struct {
	sel    types.NamespacedName
	offset int
}

func newDashboard() *dashboard { return &dashboard{} }

func (d *dashboard) typing(m *Model) bool { return m.filtering }

func (d *dashboard) crumbs(*Model) []string { return nil }

func (d *dashboard) headerLeft(m *Model) string {
	ns := m.namespace
	if m.allNamespaces {
		ns = "all"
	}
	return fmt.Sprintf("context %s  namespace %s", styleBold.Render(orDash(m.kubeContext)), styleBold.Render(ns))
}

func orDash(s string) string {
	if s == "" {
		return symEm
	}
	return s
}

func (d *dashboard) headerRight(m *Model) string {
	var parts []string
	if op := m.operator; op.Found {
		state := styleWarn.Render(symRunning + " not ready")
		if op.Ready {
			state = styleOK.Render(symOK + " ready")
		}
		parts = append(parts, "operator "+op.Version+" "+state)
	}
	if m.state != nil {
		if region := m.state.region(); region != "" {
			parts = append(parts, "B2 "+region)
		}
	}
	return strings.Join(parts, "   ")
}

// visible are the rows the filter keeps, in the chosen order.
func (d *dashboard) visible(m *Model) []*row {
	if m.state == nil {
		return nil
	}
	needle := strings.ToLower(m.filter.value)
	var out []*row
	for _, r := range m.state.rows {
		if needle == "" || strings.Contains(strings.ToLower(r.sb.Namespace+"/"+r.sb.Name), needle) {
			out = append(out, r)
		}
	}
	if m.problemsFirst {
		slices.SortStableFunc(out, func(a, b *row) int { return cmp.Compare(a.health(), b.health()) })
	}
	return out
}

func (d *dashboard) selected(m *Model) (*row, int) {
	rows := d.visible(m)
	for i, r := range rows {
		if r.key() == d.sel {
			return r, i
		}
	}
	if len(rows) > 0 {
		d.sel = rows[0].key()
		return rows[0], 0
	}
	return nil, -1
}

func (d *dashboard) top(m *Model, width int) []string {
	var left string
	switch {
	case m.watchErr != nil:
		left = styleFail.Render("  cannot watch: " + errText(m.watchErr))
	case m.stateErr != nil:
		left = styleFail.Render("  " + errText(m.stateErr))
	case m.state == nil:
		left = styleDim.Render("  loading" + symEllipsis)
	default:
		left = "  " + d.counts(m)
	}
	right := styleKey.Render("/") + " filter   " + styleKey.Render("?") + " help"
	switch {
	case m.filtering:
		right = styleKey.Render("/") + " " + m.filter.view(18) + "  " + styleDim.Render("⏎ keep  esc clear")
	case m.filter.value != "":
		right = "filter " + styleBold.Render(m.filter.value) + "  " + styleDim.Render("esc clears") + "   " +
			styleKey.Render("?") + " help"
	}
	if m.problemsFirst {
		right = styleDim.Render("problems first") + "   " + right
	}
	return []string{spread(left, right+" ", width)}
}

func (d *dashboard) counts(m *Model) string {
	var ok, running, paused, failing int
	for _, r := range m.state.rows {
		switch r.health() {
		case healthOK, healthWaiting:
			ok++
		case healthRunning:
			running++
		case healthSuspended, healthHeld:
			paused++
		default:
			failing++
		}
	}
	noun := "backups"
	if len(m.state.rows) == 1 {
		noun = "backup"
	}
	return strings.Join([]string{
		fmt.Sprintf("%d %s", len(m.state.rows), noun),
		styleOK.Render(fmt.Sprintf("%s %d healthy", symOK, ok)),
		styleWarn.Render(fmt.Sprintf("%s %d running", symRunning, running)),
		styleDim.Render(fmt.Sprintf("%s %d suspended", symPaused, paused)),
		styleFail.Render(fmt.Sprintf("%s %d failing", symFail, failing)),
	}, "   ")
}

type column struct {
	title string
	width int
	cell  func(r *row) string
}

func (d *dashboard) columns(m *Model, rows []*row, width int) []column {
	now := m.now()
	cols := []column{
		{title: "NAMESPACE", cell: func(r *row) string { return r.sb.Namespace }},
		{title: "BACKUP", cell: func(r *row) string { return r.sb.Name }},
		{title: colSources, cell: func(r *row) string { return r.sources() }},
		{title: colLastSuccess, cell: func(r *row) string {
			if t := backup.LastSuccess(r.sb); t != nil {
				return ago(t.Time, now)
			}
			return "never"
		}},
		{title: colNextRun, cell: func(r *row) string { return nextRunCell(r, now) }},
		{title: "STATUS", cell: func(r *row) string { return r.status(now) }},
	}
	if !m.allNamespaces {
		cols = cols[1:]
	}
	caps := map[string]int{"NAMESPACE": 22, "BACKUP": 18, colSources: 22, colLastSuccess: 12, colNextRun: 22,
		"STATUS": 22}
	for i := range cols {
		w := lipgloss.Width(cols[i].title)
		for _, r := range rows {
			w = max(w, lipgloss.Width(cols[i].cell(r)))
		}
		cols[i].width = min(w, caps[cols[i].title])
	}
	total := func() int {
		n := 2
		for _, c := range cols {
			n += c.width + 2
		}
		return n
	}
	for _, drop := range []string{colSources, colLastSuccess, colNextRun} {
		if total() <= width {
			break
		}
		cols = slices.DeleteFunc(cols, func(c column) bool { return c.title == drop })
	}
	for total() > width {
		widest := 0
		for i := range cols[:len(cols)-1] {
			if cols[i].width > cols[widest].width {
				widest = i
			}
		}
		if cols[widest].width <= 8 {
			break
		}
		cols[widest].width--
	}
	return cols
}

func nextRunCell(r *row, now time.Time) string {
	next, ok := r.nextRun(now)
	if !ok {
		return symEm
	}
	at := next.Format("15:04")
	if next.Sub(now) >= 24*time.Hour {
		at = next.Format("01-02 15:04")
	}
	return at + "  in " + span(next.Sub(now))
}

func (d *dashboard) body(m *Model, width, height int) []string {
	if m.state == nil {
		return nil
	}
	rows := d.visible(m)
	if len(rows) == 0 {
		msg := "  No ScheduledBackups in namespace " + m.namespace + "."
		switch {
		case m.filter.value != "":
			msg = "  Nothing matches the filter " + m.filter.value + "."
		case m.allNamespaces:
			msg = "  No ScheduledBackups in any namespace."
		}
		if !m.allNamespaces && m.filter.value == "" {
			msg += " A shows every namespace."
		}
		return []string{"", styleDim.Render(msg)}
	}
	cols := d.columns(m, rows, width)
	_, sel := d.selected(m)
	space := max(1, height-1)
	if len(rows) > space {
		space = max(1, space-1)
	}
	d.offset = clampOffset(d.offset, sel, space, len(rows))

	var header strings.Builder
	header.WriteString("  ")
	for _, c := range cols {
		header.WriteString(fit(c.title, c.width) + "  ")
	}
	out := []string{styleDim.Render(header.String())}
	for i := d.offset; i < min(len(rows), d.offset+space); i++ {
		r := rows[i]
		line := "  "
		if i == sel {
			line = styleAccent.Render(symCursor) + " "
		}
		for j, c := range cols {
			cell := fit(c.cell(r), c.width)
			if i == sel && j < len(cols)-1 {
				cell = styleSelected.Render(cell)
			}
			line += cell + "  "
		}
		out = append(out, line)
	}
	if more := len(rows) - d.offset - space; more > 0 {
		out = append(out, styleDim.Render(fmt.Sprintf("  %s %d more · PgDn", symEllipsis, more)))
	}
	return out
}

func clampOffset(offset, sel, space, n int) int {
	if sel >= 0 {
		if sel < offset {
			offset = sel
		}
		if sel >= offset+space {
			offset = sel - space + 1
		}
	}
	return max(0, min(offset, n-space))
}

// strip shows the backup in flight, with restic's progress when the TUI
// follows its logs.
func (d *dashboard) strip(m *Model, width int) string {
	if m.state == nil {
		return ""
	}
	sel, _ := d.selected(m)
	var r *row
	if sel != nil && sel.running() != nil {
		r = sel
	} else {
		for _, cand := range d.visible(m) {
			if cand.running() != nil {
				r = cand
				break
			}
		}
	}
	if r == nil {
		return ""
	}
	return runStrip(m, r, width)
}

func runStrip(m *Model, r *row, width int) string {
	job := r.running()
	loc := r.location()
	by := "on schedule"
	if m.mine(r, job.Name) {
		by = "by you"
	} else if job.Labels[backup.LabelTrigger] == backup.TriggerManual {
		by = "by hand"
	}
	text := fmt.Sprintf(" %s  backup started %s %s", styleBold.Render(r.title()),
		jobStart(job).In(loc).Format("15:04:05"), by)
	if s, ok := m.streams[r.sb.Namespace+"/"+job.Name]; ok {
		if p := s.snapshot().progress; p != nil {
			text += "   " + progressText(p, 0)
		}
	} else {
		text += "   " + styleWarn.Render("running "+clock(m.now().Sub(jobStart(job))))
	}
	return trunc(text, width)
}

// progressText renders restic's progress, with a bar when width is set.
func progressText(p *progress, barWidth int) string {
	var parts []string
	if p.hasPercent {
		if barWidth > 0 {
			parts = append(parts, bar(p.percent/100, barWidth))
		}
		parts = append(parts, fmt.Sprintf("restic %.0f%%", p.percent))
	}
	parts = append(parts, p.done+" of "+p.total)
	if p.eta > 0 {
		parts = append(parts, "ETA "+clock(p.eta))
	}
	return strings.Join(parts, "  ")
}

// bar draws a fraction as a bar width cells wide, in eighths.
func bar(frac float64, width int) string {
	frac = max(0, min(1, frac))
	eighths := int(frac * float64(width*8))
	full, part := eighths/8, eighths%8
	s := strings.Repeat(symBar, full)
	if part > 0 && full < width {
		s += string([]rune(" ▏▎▍▌▋▊▉")[part])
		full++
	}
	return styleAccent.Render(s) + styleDim.Render(strings.Repeat(symTrack, width-full))
}

// pauseLabel names what p does to the backup.
func pauseLabel(r *row) string {
	if r != nil && r.sb.Spec.Suspend {
		return "resume"
	}
	return "pause"
}

func (d *dashboard) bindings(m *Model) []binding {
	r, _ := d.selected(m)
	return []binding{
		{keys: []string{keyUp}, disp: "↑↓", label: "move"},
		{keys: []string{keyEnter}, disp: "⏎", label: "open"},
		{disp: "b", label: wordBackupNow, act: actBackup},
		{disp: "l", label: "logs", act: actLogs},
		{disp: "s", label: wordSnapshots, act: actPod},
		{disp: "d", label: wordDoctor},
		{disp: "p", label: pauseLabel(r), act: actSuspend},
		{disp: "r", label: wordRestore, act: actPod},
		{disp: "A", label: "all ns"},
		{disp: "q", label: "quit"},
		{disp: "c", label: "integrity check, optionally reading data", act: actPod, helpOnly: true},
		{disp: "x", label: "cancel the running backup", act: actCancel, helpOnly: true},
		{disp: "S", label: "problems first, or by namespace", helpOnly: true},
		{disp: "/", label: "filter by namespace or name", helpOnly: true},
		{disp: "PgUp PgDn", label: "page", helpOnly: true},
	}
}

func (d *dashboard) handleKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	k := msg.String()
	if m.filtering {
		switch k {
		case keyEsc:
			m.filtering, m.filter.focused, m.filter.value = false, false, ""
		case keyEnter, keyDown, keyUp:
			m.filtering, m.filter.focused = false, false
		default:
			m.filter.edit(msg)
		}
		return nil, true
	}
	rows := d.visible(m)
	r, sel := d.selected(m)
	move := func(to int) {
		if len(rows) > 0 {
			d.sel = rows[max(0, min(len(rows)-1, to))].key()
		}
	}
	switch k {
	case keyUp, "k":
		move(sel - 1)
	case keyDown, "j":
		move(sel + 1)
	case keyPgUp:
		move(sel - max(1, m.frame().bodyH-2))
	case keyPgDown:
		move(sel + max(1, m.frame().bodyH-2))
	case "home", "g":
		move(0)
	case "end", "G":
		move(len(rows) - 1)
	case "/":
		m.filtering, m.filter.focused = true, true
	case keyEsc:
		if m.filter.value == "" {
			return nil, false
		}
		m.filter.value = ""
	case "S":
		m.problemsFirst = !m.problemsFirst
	case "A":
		m.allNamespaces = !m.allNamespaces
		d.offset = 0
		return m.reviewAccess(), true
	default:
		if r == nil {
			return nil, k != keyEsc
		}
		return d.act(m, r, k)
	}
	return nil, true
}

func (d *dashboard) act(m *Model, r *row, k string) (tea.Cmd, bool) {
	switch k {
	case keyEnter, keyRight:
		return m.openDetail(r), true
	case "b":
		return m.backupNow(r), true
	case "l":
		return m.openLogs(r, ""), true
	case "s":
		return m.openSnapshots(r), true
	case "d":
		return m.openDoctor(r), true
	case "p":
		return m.togglePause(r), true
	case "r":
		return m.openRestore(r, nil, ""), true
	case "c":
		return m.checkPrompt(r), true
	case "x":
		return m.cancelRun(r), true
	}
	return nil, false
}

func (d *dashboard) click(m *Model, _, y int) tea.Cmd {
	rows := d.visible(m)
	i := d.offset + y - 1
	if y < 1 || i >= len(rows) || i >= d.offset+max(1, m.frame().bodyH-1) {
		return nil
	}
	if rows[i].key() == d.sel {
		return m.openDetail(rows[i])
	}
	d.sel = rows[i].key()
	return nil
}

func (d *dashboard) scroll(m *Model, delta int) {
	rows := d.visible(m)
	_, sel := d.selected(m)
	if len(rows) > 0 {
		d.sel = rows[max(0, min(len(rows)-1, sel+delta))].key()
	}
}
