package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

const labelWidth = 12

// detail is everything b2bk status shows about one backup, with its runs as a
// duration chart.
type detail struct {
	key    types.NamespacedName
	sel    int
	runTop int
}

func newDetail(k types.NamespacedName) *detail { return &detail{key: k} }

func (d *detail) typing(*Model) bool { return false }

func (d *detail) crumbs(*Model) []string { return []string{d.key.Namespace, d.key.Name} }

func (d *detail) headerRight(m *Model) string {
	r := m.rowFor(d.key)
	if r == nil {
		return styleFail.Render("gone") + "   " + styleKey.Render(keyEsc) + " back"
	}
	return r.status(m.now()) + "     " + styleKey.Render(keyEsc) + " back"
}

func (d *detail) top(*Model, int) []string { return nil }

func (d *detail) strip(*Model, int) string { return "" }

// run is one backup run, from the status history or a Job still present.
type run struct {
	job     string
	trigger string
	result  b2backupv1.BackupRunResult
	start   time.Time
	end     time.Time
	reason  string
}

func (r *run) duration(now time.Time) time.Duration {
	end := r.end
	if end.IsZero() {
		end = now
	}
	if r.start.IsZero() {
		return 0
	}
	return end.Sub(r.start)
}

func runsOf(m *Model, r *row) []run {
	var runs []run
	seen := map[string]bool{}
	for _, h := range r.sb.Status.History {
		ru := run{job: h.JobName, trigger: strings.ToLower(string(h.Trigger)), result: h.Result}
		if h.StartTime != nil {
			ru.start = h.StartTime.Time
		}
		if h.CompletionTime != nil {
			ru.end = h.CompletionTime.Time
		}
		runs = append(runs, ru)
		seen[h.JobName] = true
	}
	for i := range r.jobs {
		job := &r.jobs[i]
		if seen[job.Name] {
			continue
		}
		ru := run{job: job.Name, trigger: "scheduled", start: jobStart(job)}
		if job.Labels[backup.LabelTrigger] == backup.TriggerManual {
			ru.trigger = "manual"
		}
		switch {
		case job.Status.Succeeded > 0:
			ru.result = b2backupv1.BackupRunSucceeded
		case job.Status.Failed > 0:
			ru.result = b2backupv1.BackupRunFailed
		default:
			ru.result = b2backupv1.BackupRunRunning
		}
		if job.Status.CompletionTime != nil {
			ru.end = job.Status.CompletionTime.Time
		}
		runs = append(runs, ru)
	}
	jobs := map[string]int{}
	for i := range r.jobs {
		jobs[r.jobs[i].Name] = i
	}
	for i := range runs {
		if runs[i].result != b2backupv1.BackupRunFailed {
			continue
		}
		if line := m.lastLines[runs[i].job]; line != "" {
			runs[i].reason = line
		} else if j, ok := jobs[runs[i].job]; ok {
			runs[i].reason = jobExit(&r.jobs[j], r.pods[runs[i].job])
			if runs[i].end.IsZero() {
				for _, c := range r.jobs[j].Status.Conditions {
					if c.Type == "Failed" {
						runs[i].end = c.LastTransitionTime.Time
					}
				}
			}
		}
	}
	slices.SortStableFunc(runs, func(a, b run) int { return b.start.Compare(a.start) })
	return runs
}

func (d *detail) body(m *Model, width, height int) []string {
	r := m.rowFor(d.key)
	if r == nil {
		return []string{"", styleDim.Render("  scheduledbackup/" + d.key.Name + " is gone.")}
	}
	now := m.now()
	var out []string
	add := func(label, value, right string) {
		line := "  " + fit(styleBold.Render(label), labelWidth) + value
		if right != "" {
			line = spread(line, styleDim.Render(right)+"  ", width)
		}
		out = append(out, line)
	}
	add("SCHEDULE", scheduleText(r), nextText(r, now))
	for i, src := range sourceLines(r.sb) {
		label := "SOURCE"
		if i > 0 {
			label = ""
		}
		add(label, src, "")
	}
	scope := fmt.Sprintf("forget scoped to host %s", r.sb.EffectiveResticHost())
	if tags := backup.SourceTags(r.sb.Spec.Sources); len(tags) > 0 {
		scope += ", tag " + strings.Join(tags, ", ")
	}
	add("RETENTION", retentionText(r.sb.Spec.Retention), scope)
	add("REPOSITORY", repositoryText(r), "")
	add("LIFECYCLE", lifecycleText(r), "")
	add("STORED", storedText(m, r, now), "")
	if r.hold != "" {
		add("HELD", styleWarn.Render(r.hold), "")
	}

	out = append(out, "", "  "+styleBold.Render("CONDITIONS"))
	conds := slices.Clone(r.sb.Status.Conditions)
	if r.repo != nil {
		if c := apimeta.FindStatusCondition(r.repo.Status.Conditions, b2backupv1.RepositoryConditionReady); c != nil {
			rc := *c
			rc.Type = "Repository"
			conds = append(conds, rc)
		}
	}
	if len(conds) == 0 {
		out = append(out, styleDim.Render("  the operator has not reconciled this backup yet"))
	}
	for _, c := range conds {
		sym := styleOK.Render(symOK)
		switch c.Status {
		case metav1.ConditionFalse:
			sym = styleFail.Render(symFail)
		case metav1.ConditionUnknown:
			sym = styleDim.Render(symWaiting)
		}
		out = append(out, fmt.Sprintf("  %s %s %s %s %s", sym, fit(c.Type, 12), fit(string(c.Status), 6),
			fit(c.Reason, 18), styleDim.Render(oneLine(c.Message))))
	}

	runs := runsOf(m, r)
	out = append(out, "")
	space := height - len(out) - 1
	return append(out, d.runLines(m, r, runs, width, space)...)
}

func scheduleText(r *row) string {
	text := r.sb.Spec.Schedule
	if eff := r.sb.Status.EffectiveSchedule; eff != "" && eff != text {
		text += " → " + eff
	}
	tz := r.sb.Spec.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	return text + "  " + styleDim.Render(tz)
}

func nextText(r *row, now time.Time) string {
	next, ok := r.nextRun(now)
	switch {
	case ok:
		return "next " + dayWord(next, now.In(next.Location())) + " " + next.Format("15:04") +
			" (in " + span(next.Sub(now)) + ")"
	case r.hold != "":
		return "not firing: held for a restore"
	case r.sb.Spec.Suspend:
		return "not firing: suspended"
	}
	return "not firing"
}

func dayWord(t, now time.Time) string {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "today"
	case y1 == y2 && m1 == m2 && d1 == d2+1:
		return "tomorrow"
	}
	return t.Format("Mon 01-02")
}

func sourceLines(sb *b2backupv1.ScheduledBackup) []string {
	if len(sb.Spec.Sources) == 0 {
		if sb.Spec.Script != "" {
			return []string{fmt.Sprintf("script  %s", styleDim.Render(fmt.Sprintf("%d lines, no source tags",
				strings.Count(strings.TrimRight(sb.Spec.Script, "\n"), "\n")+1)))}
		}
		return []string{symEm}
	}
	var out []string
	for i := range sb.Spec.Sources {
		src := &sb.Spec.Sources[i]
		tag := src.EffectiveTag()
		if src.Type == b2backupv1.SourceTypeFiles {
			line := tag + "  " + strings.Join(src.ResolvedPaths(sb.EffectiveWorkingDir()), ", ")
			if len(src.Exclude) > 0 {
				line += "  " + styleDim.Render("excluding "+strings.Join(src.Exclude, ", "))
			}
			out = append(out, line)
			continue
		}
		out = append(out, tag+"  "+databaseText(sb, src))
	}
	return out
}

func databaseText(sb *b2backupv1.ScheduledBackup, src *b2backupv1.BackupSource) string {
	db := sb.Spec.Database
	name := src.Database
	host := sb.EffectiveResticHost()
	if db == nil {
		return string(src.Type)
	}
	if name == "" {
		name = db.Name
	}
	if name == "" {
		name = "the Secret's dbname"
	}
	switch db.Engine {
	case b2backupv1.DatabaseEngineMariaDB:
		return fmt.Sprintf("mariadb %s / %s  → /%s.sql", orDash(db.Host), name, host)
	default:
		return fmt.Sprintf("cnpg %s / %s  → /%s.dmp", db.EffectiveCNPGCluster(), name, host)
	}
}

func retentionText(r *b2backupv1.Retention) string {
	if r == nil {
		return styleDim.Render("none: no snapshot is ever forgotten")
	}
	var parts []string
	for _, f := range []struct {
		name string
		v    *int32
	}{{"last", r.Last}, {"hourly", r.Hourly}, {"daily", r.Daily}, {"weekly", r.Weekly}, {"monthly", r.Monthly},
		{"yearly", r.Yearly}} {
		if f.v != nil {
			parts = append(parts, fmt.Sprintf("%s %d", f.name, *f.v))
		}
	}
	return strings.Join(parts, " · ")
}

func repositoryText(r *row) string {
	if r.repo == nil {
		return styleDim.Render(r.sb.Spec.RepositoryRef.Name + " (not readable)")
	}
	st := r.repo.Status
	where := st.BucketName
	if st.Path != "" {
		where += "/" + st.Path
	}
	if where == "" {
		where = r.repo.Name + styleDim.Render(" (no bucket recorded yet)")
	}
	parts := []string{where}
	if st.Adopted {
		parts = append(parts, "adopted")
	}
	if id := st.ApplicationKeyID; len(id) > 8 {
		parts = append(parts, "key "+id[:4]+symEllipsis+id[len(id)-4:])
	}
	return strings.Join(parts, "   ")
}

func lifecycleText(r *row) string {
	if r.repo == nil {
		return symEm
	}
	c := apimeta.FindStatusCondition(r.repo.Status.Conditions, b2backupv1.RepositoryConditionLifecycleRule)
	if c == nil {
		return styleDim.Render("not reported")
	}
	text := oneLine(c.Message)
	if text == "" {
		text = c.Reason
	}
	if c.Status != metav1.ConditionTrue {
		return styleWarn.Render(text)
	}
	return text
}

func storedText(m *Model, r *row, now time.Time) string {
	var parts []string
	if r.repo != nil && r.repo.Status.CurrentUsage != "" {
		text := r.repo.Status.CurrentUsage
		if t := r.repo.Status.UsageTime; t != nil {
			text += styleDim.Render(" (measured " + ago(t.Time, now) + ")")
		}
		parts = append(parts, text)
	}
	if s := m.sessions.get(r.key()); s != nil {
		if v := s.view(); v.snapshotsDone && v.snapshotsErr == nil {
			parts = append(parts, fmt.Sprintf("%d snapshots", len(v.snapshots)))
		}
	}
	if len(parts) == 0 {
		return styleDim.Render("not measured yet")
	}
	return strings.Join(parts, " · ")
}

func (d *detail) runLines(m *Model, r *row, runs []run, width, space int) []string {
	now := m.now()
	var longest time.Duration
	for i := range runs {
		longest = max(longest, runs[i].duration(now))
	}
	scale := niceScale(longest)
	barWidth := 16
	head := "  " + styleBold.Render("RECENT RUNS")
	legend := styleDim.Render("duration 0 " + strings.Repeat(symRule, 7) + " " + clock(scale))
	out := []string{spread(head, legend+"  ", width)}
	if len(runs) == 0 {
		return append(out, styleDim.Render("  no runs recorded yet"))
	}
	d.sel = max(0, min(d.sel, len(runs)-1))
	space = max(1, space)
	d.runTop = clampOffset(d.runTop, d.sel, space, len(runs))
	loc := r.location()
	jobWidth := 8
	for i := range runs {
		jobWidth = max(jobWidth, min(26, len(runs[i].job)))
	}
	for i := d.runTop; i < min(len(runs), d.runTop+space); i++ {
		ru := &runs[i]
		sym := styleOK.Render(symSucceeded)
		switch ru.result {
		case b2backupv1.BackupRunFailed:
			sym = styleFail.Render(symFail)
		case b2backupv1.BackupRunRunning:
			sym = styleWarn.Render(symRunning)
		}
		cursor := " "
		if i == d.sel {
			cursor = styleAccent.Render(symCursor)
		}
		when := symEm
		if !ru.start.IsZero() {
			when = ru.start.In(loc).Format("Mon 01-02 15:04")
		}
		dur := ru.duration(now)
		line := fmt.Sprintf(" %s%s %s  %s  %s  %s  %s", cursor, sym, fit(when, 15), fit(ru.trigger, 9),
			fit(ru.job, jobWidth), fmt.Sprintf("%6s", clock(dur)), bar(float64(dur)/float64(scale), barWidth))
		if i == d.sel {
			line = styleSelected.Render(line)
		}
		if ru.reason != "" {
			if lipgloss.Width(line)+2+lipgloss.Width(ru.reason) <= width {
				line += "  " + styleFail.Render(ru.reason)
			} else {
				out = append(out, line)
				line = "      " + styleFail.Render("↳ "+trunc(ru.reason, width-9))
			}
		}
		out = append(out, line)
	}
	return lines(out, len(out))
}

// niceScale rounds a longest duration up to a round chart scale.
func niceScale(d time.Duration) time.Duration {
	for _, s := range []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour} {
		if d <= s {
			return s
		}
	}
	return 24 * time.Hour
}

func (d *detail) bindings(m *Model) []binding {
	return []binding{
		{disp: "b", label: wordBackupNow, act: actBackup},
		{disp: "l", label: "logs of selected run", act: actLogs},
		{disp: "s", label: wordSnapshots, act: actPod},
		{disp: "d", label: wordDoctor},
		{disp: "p", label: pauseLabel(m.rowFor(d.key)), act: actSuspend},
		{disp: "r", label: wordRestore, act: actPod},
		{disp: "c", label: "check", act: actPod},
		{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
		{keys: []string{keyUp}, disp: "↑↓", label: "choose a run", helpOnly: true},
		{disp: "x", label: "cancel the running backup", act: actCancel, helpOnly: true},
	}
}

func (d *detail) handleKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	r := m.rowFor(d.key)
	if r == nil {
		return nil, false
	}
	switch msg.String() {
	case keyUp, "k":
		d.sel = max(0, d.sel-1)
	case keyDown, "j":
		d.sel++
	case keyEnter, "l":
		runs := runsOf(m, r)
		if len(runs) == 0 {
			return m.openLogs(r, ""), true
		}
		return m.openLogs(r, runs[max(0, min(d.sel, len(runs)-1))].job), true
	case "b":
		return m.backupNow(r), true
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
	default:
		return nil, false
	}
	return nil, true
}

func (d *detail) click(m *Model, _, y int) tea.Cmd {
	r := m.rowFor(d.key)
	if r == nil {
		return nil
	}
	lines := d.body(m, m.width, m.frame().bodyH)
	for i, line := range lines {
		if strings.Contains(line, "RECENT RUNS") {
			idx := d.runTop + y - i - 1
			if y > i && idx < len(runsOf(m, r)) {
				if idx == d.sel {
					return m.openLogs(r, runsOf(m, r)[idx].job)
				}
				d.sel = idx
			}
			return nil
		}
	}
	return nil
}

func (d *detail) scroll(_ *Model, delta int) { d.sel = max(0, d.sel+delta) }
