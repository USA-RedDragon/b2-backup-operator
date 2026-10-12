package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func dashboardOn(t *testing.T, b *fakeBackend, ns string) *Model {
	t.Helper()
	m := newTestModel(t, b, allNamespaces)
	start(t, m)
	d := m.top().(*dashboard)
	d.sel = key(ns)
	return m
}

func calls(b *fakeBackend, prefix string) []string {
	var out []string
	for _, c := range b.recorded() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestBackupNowTakesOneKeyToConfirm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		answer string
		want   int
	}{{"n", 0}, {"esc", 0}, {"Y", 0}, {"y", 1}}
	for _, tt := range tests {
		t.Run(tt.answer, func(t *testing.T) {
			t.Parallel()
			b := newFakeBackend(t)
			m := dashboardOn(t, b, nsKosync)
			press(t, m, "b")
			if m.prompt == nil || !strings.Contains(screenText(m), "Back up kosync/restic now?") {
				t.Fatalf("b did not ask first:\n%s", screenText(m))
			}
			if got := calls(b, "backup"); len(got) != 0 {
				t.Fatalf("b backed up before the answer: %v", got)
			}
			press(t, m, tt.answer)
			if got := calls(b, "backup "+nsKosync+"/"+nameRestic); len(got) != tt.want {
				t.Errorf("after %q: backups = %v, want %d", tt.answer, got, tt.want)
			}
		})
	}
}

func TestBackupNowMarksTheRunAsYours(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := dashboardOn(t, b, nsGitea)
	r := m.rowFor(key(nsGitea))
	if m.mine(r, "restic-manual-x8k2qp") {
		t.Fatal("a run this TUI did not trigger counts as yours")
	}
	m.triggered[key(nsGitea)] = r.sb.Status.LastTriggerTime.Add(-500 * 1e6)
	if !m.mine(r, "restic-manual-x8k2qp") {
		t.Error("the run of a trigger this TUI sent does not count as yours")
	}
}

func TestPauseAndResumeFollowSpecSuspend(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := dashboardOn(t, b, nsKosync)
	press(t, m, "p")
	m.top().(*dashboard).sel = key("wx")
	press(t, m, "p")
	want := []string{"suspend kosync/restic true", "suspend wx/restic false"}
	if got := calls(b, "suspend"); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestCancelAsksAndNeedsARunningBackup(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := dashboardOn(t, b, nsKosync)
	press(t, m, "x")
	if m.prompt != nil || len(calls(b, "cancel")) != 0 {
		t.Fatal("x offered to cancel a backup that is not running")
	}
	m.top().(*dashboard).sel = key(nsGitea)
	press(t, m, "x", "n")
	if len(calls(b, "cancel")) != 0 {
		t.Fatal("n cancelled the run")
	}
	press(t, m, "x", "y")
	if got := calls(b, "cancel"); !slices.Equal(got, []string{"cancel gitea/restic"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestDeniedActionsAreHiddenAndRefused(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.denied["patch scheduledbackups"] = true
	m := dashboardOn(t, b, nsKosync)
	footer := m.footer(m.top())
	if strings.Contains(footer, "backup now") || strings.Contains(footer, "pause") {
		t.Errorf("the footer offers what RBAC denies: %s", footer)
	}
	press(t, m, "b", "y", "p")
	if got := calls(b, "backup"); len(got) != 0 {
		t.Errorf("a denied backup ran: %v", got)
	}
	if got := calls(b, "suspend"); len(got) != 0 {
		t.Errorf("a denied pause ran: %v", got)
	}
	if !strings.Contains(m.flash, "patch scheduledbackups") {
		t.Errorf("flash = %q, want the RBAC reason", m.flash)
	}
}

func TestAllNamespacesToggle(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := newTestModel(t, b, Options{Mouse: true})
	start(t, m)
	if n := len(m.state.rows); n != 6 {
		t.Logf("the fake reader ignores the namespace, so the scope is checked on the model, not the rows (%d)", n)
	}
	if m.scope() != nsVault || strings.Contains(screenText(m), "NAMESPACE ") {
		t.Fatalf("scope = %q, want the current namespace and no NAMESPACE column", m.scope())
	}
	gen := m.watchGen
	press(t, m, "A")
	if m.scope() != "" || !m.allNamespaces {
		t.Fatalf("A left the scope at %q", m.scope())
	}
	if m.watchGen == gen {
		t.Error("A did not restart the watch")
	}
	if !strings.Contains(screenText(m), "namespace all") {
		t.Errorf("the header does not say all namespaces:\n%s", screenText(m))
	}
}

func TestMouseToggle(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeBackend(t), allNamespaces)
	start(t, m)
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("the mouse is not captured by default")
	}
	press(t, m, "m")
	if m.View().MouseMode != tea.MouseModeNone {
		t.Error("m did not release the mouse")
	}
	press(t, m, "m")
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Error("m did not capture the mouse again")
	}

	off := newTestModel(t, newFakeBackend(t), Options{AllNamespaces: true})
	if off.View().MouseMode != tea.MouseModeNone {
		t.Error("Mouse: false still captures the mouse")
	}
}

func click(t *testing.T, m *Model, x, y int) {
	t.Helper()
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	settle(t, m, cmd)
}

func TestClickSelectsThenOpens(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeBackend(t), allNamespaces)
	start(t, m)
	top := m.frame().bodyTop
	click(t, m, 10, top+3)
	if got := m.top().(*dashboard).sel; got != key(nsKosync) {
		t.Fatalf("the click selected %v, want kosync", got)
	}
	click(t, m, 10, top+3)
	d, ok := m.top().(*detail)
	if !ok || d.key != key(nsKosync) {
		t.Fatalf("a second click did not open kosync: %T", m.top())
	}
}

func TestWheelMovesTheSelection(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeBackend(t), allNamespaces)
	start(t, m)
	_, _ = m.Update(tea.MouseWheelMsg{X: 5, Y: 6, Button: tea.MouseWheelDown})
	if got := m.top().(*dashboard).sel; got != key(nsMinecraft) {
		t.Errorf("the wheel moved the selection to %v, want three rows down", got)
	}
}

func TestFooterClickActsAsItsKey(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := dashboardOn(t, b, nsKosync)
	footer := screenText(m)
	footer = footer[strings.LastIndex(footer, "\n")+1:]
	x := strings.Index(footer, "d doctor")
	if x < 0 {
		t.Fatalf("no doctor in the footer: %q", footer)
	}
	click(t, m, len([]rune(footer[:x])), m.height-1)
	if _, ok := m.top().(*doctorScreen); !ok {
		t.Errorf("clicking d doctor opened %T", m.top())
	}
}

func TestEscGoesBackAndQQuits(t *testing.T) {
	t.Parallel()
	m := dashboardOn(t, newFakeBackend(t), nsKosync)
	press(t, m, "enter", "d")
	if len(m.screens) != 3 {
		t.Fatalf("screens = %d, want dashboard, detail, doctor", len(m.screens))
	}
	press(t, m, "esc", "esc")
	if len(m.screens) != 1 {
		t.Fatalf("esc twice left %d screens", len(m.screens))
	}
	_, cmd := m.Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("q did nothing")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q did not quit")
	}
}

func TestQuitAsksWhileARestoreWrites(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := dashboardOn(t, b, nsVault)
	w := newRestoreScreen(m.rowFor(key(nsVault)), &b.snapshots[0], tagFiles)
	w.step = stepRunning
	w.run = &stream{cancel: func() {}}
	m.push(w)
	_, cmd := m.Update(keyMsg("q"))
	if cmd != nil {
		t.Fatal("q quit while a restore was writing")
	}
	if m.prompt == nil || !strings.Contains(m.prompt.text, "A restore is running") {
		t.Fatalf("q did not warn: %+v", m.prompt)
	}
	_, cmd = m.Update(keyMsg("q"))
	if cmd == nil {
		t.Fatal("a second q did not quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("a second q did not quit")
	}
}

func TestCloseRemovesEverySessionPod(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := New(t.Context(), b, allNamespaces)
	start(t, m)
	m.top().(*dashboard).sel = key(nsVault)
	press(t, m, "s")
	eventually(t, func() bool { return m.sessions.get(key(nsVault)).view().state == podReady })
	press(t, m, "esc", "s")
	if got := calls(b, "open"); len(got) != 1 {
		t.Errorf("the pod was started %d times, want once per backup per session", len(got))
	}
	m.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed != 1 {
		t.Errorf("closed %d pods, want 1", b.closed)
	}
}

func TestFilterAndProblemsFirst(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeBackend(t), allNamespaces)
	start(t, m)
	press(t, m, "/")
	typeText(t, m, "ko")
	press(t, m, "enter")
	d := m.top().(*dashboard)
	if rows := d.visible(m); len(rows) != 1 || rows[0].key() != key(nsKosync) {
		t.Fatalf("filter ko kept %d rows", len(rows))
	}
	press(t, m, "q")
	if m.filtering {
		t.Fatal("enter did not leave the filter")
	}
	press(t, m, "esc")
	if len(d.visible(m)) != 6 || m.filter.value != "" {
		t.Fatal("esc did not clear the filter")
	}
	press(t, m, "S")
	rows := d.visible(m)
	if rows[0].key() != key(nsMinecraft) || rows[len(rows)-1].health() != healthOK {
		t.Errorf("S did not put the failing backup first: %v", rows[0].key())
	}
}

func TestDoctorActsOnTheSelectedCheck(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := dashboardOn(t, b, nsMinecraft)
	press(t, m, "d")
	d := m.top().(*doctorScreen)
	if len(d.checks) == 0 || d.checks[0].Level != LevelFail || d.checks[1].Level != LevelWarn {
		t.Fatalf("checks are not failing first: %+v", d.checks)
	}
	press(t, m, "enter")
	if _, ok := m.top().(*logsScreen); !ok {
		t.Errorf("enter on a failed run opened %T, want its logs", m.top())
	}
	if got := calls(b, "doctor"); !slices.Equal(got, []string{"doctor minecraft/restic"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestResizeRedraws(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeBackend(t), allNamespaces)
	start(t, m)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 64, Height: 16})
	for line := range strings.SplitSeq(screenText(m), "\n") {
		if w := len([]rune(line)); w > 64 {
			t.Errorf("a line is %d cells wide after resizing to 64: %q", w, line)
		}
	}
	if got := strings.Count(screenText(m), "\n") + 1; got != 16 {
		t.Errorf("the frame is %d lines, want 16", got)
	}
}

func TestLogsSwitchesBetweenRuns(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := dashboardOn(t, b, nsKosync)
	press(t, m, "enter", "down", "l")
	s, ok := m.top().(*logsScreen)
	if !ok || s.job != "restic-manual-tmrg1h" {
		t.Fatalf("l on the second run opened %+v", m.top())
	}
	press(t, m, "[")
	if s.job != "restic-backup-29340715" {
		t.Errorf("[ moved to %s, want the older run", s.job)
	}
	press(t, m, "]", "]")
	if s.job != "restic-backup-29342155" {
		t.Errorf("] moved to %s, want the newest run", s.job)
	}
}

func TestCheckRunsWithTheReadDataSubset(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := dashboardOn(t, b, nsKosync)
	press(t, m, "c")
	typeText(t, m, "1/10")
	press(t, m, "enter")
	eventually(t, func() bool { return len(calls(b, "check")) == 1 })
	if got := calls(b, "check"); got[0] != "check kosync/sb/restic 1/10" {
		t.Errorf("calls = %v", got)
	}
}

func TestCloseWaitsForBackgroundWorkToCleanUp(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.checkBlocks = true
	m := New(t.Context(), b, allNamespaces)
	start(t, m)
	m.top().(*dashboard).sel = key(nsKosync)
	press(t, m, "c", "enter")
	eventually(t, func() bool { return len(calls(b, "check kosync")) == 1 })
	m.Close()
	if got := calls(b, "check cleaned up"); len(got) != 1 {
		t.Error("Close returned before the check it started had cleaned up")
	}
}

func TestWithoutScheduledBackupsTheDashboardSaysWhy(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.denied["watch scheduledbackups"] = true
	m := newTestModel(t, b, allNamespaces)
	start(t, m)
	if m.stateErr != errNoBackups || !strings.Contains(screenText(m), "RBAC does not let you list and watch") {
		t.Errorf("stateErr = %v; screen:\n%s", m.stateErr, screenText(m))
	}
}
