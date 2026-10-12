package tui

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"
)

// program runs the TUI as bubbletea does, on the fake backend at the fixed
// clock, and lets a test wait for what it draws.
type program struct {
	t     *testing.T
	tm    *teatest.TestModel
	frame atomic.Value
}

func runProgram(t *testing.T, b *fakeBackend, opts Options, width, height int) *program {
	t.Helper()
	m := New(t.Context(), b, opts)
	m.now = func() time.Time { return testNow }
	t.Cleanup(m.Close)
	p := &program{t: t}
	p.frame.Store("")
	record := func(model tea.Model, msg tea.Msg) tea.Msg {
		p.frame.Store(screenText(model.(*Model)))
		return msg
	}
	p.tm = teatest.NewTestModel(t, m, teatest.WithInitialTermSize(width, height),
		teatest.WithProgramOptions(tea.WithFilter(record)))
	return p
}

// waitFor waits until the screen shows every one of want.
func (p *program) waitFor(want ...string) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		frame := p.frame.Load().(string)
		missing := ""
		for _, w := range want {
			if !strings.Contains(frame, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("the screen never showed %q; it shows:\n%s", missing, frame)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *program) press(keys ...string) {
	for _, k := range keys {
		p.tm.Send(keyMsg(k))
	}
}

func (p *program) typeText(s string) {
	for _, r := range s {
		p.tm.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// golden quits and compares the last frame with testdata/<test>.golden; run
// go test with -update to rewrite them.
func (p *program) golden() {
	p.t.Helper()
	if err := p.tm.Quit(); err != nil {
		p.t.Fatal(err)
	}
	m := p.tm.FinalModel(p.t, teatest.WithFinalTimeout(10*time.Second)).(*Model)
	golden.RequireEqual(p.t, screenText(m))
}

var allNamespaces = Options{AllNamespaces: true, Mouse: true}

func TestGoldenDashboard(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "wx", "B2 us-east-005")
	p.golden()
}

func TestGoldenDashboardCurrentNamespace(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), Options{Mouse: true}, frameWidth, frameHeight)
	p.waitFor("1 backup ", "files /data, db")
	p.golden()
}

func TestGoldenDashboardNarrow(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, 72, 20)
	p.waitFor("6 backups", "wx")
	p.golden()
}

func TestGoldenDashboardProblemsFirstFiltered(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "wx")
	p.press("S", "/")
	p.typeText("i")
	p.waitFor("problems first", "[ i")
	p.golden()
}

func TestGoldenDetail(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "kosync")
	p.press("j", "j", "enter")
	p.waitFor("RECENT RUNS", "restic-backup-29336395")
	p.golden()
}

func TestGoldenDetailFailedRun(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "minecraft")
	p.press("j", "j", "j", "enter")
	p.waitFor("RECENT RUNS", "permission denied")
	p.golden()
}

func TestGoldenLogs(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "gitea")
	p.press("j", "l")
	p.waitFor("61%", "file changed during read")
	p.golden()
}

func TestGoldenSnapshotsStarting(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.podGate = make(chan struct{})
	p := runProgram(t, b, allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "vaultwarden")
	p.press("G", "k", "s")
	p.waitFor("Starting the restic pod", "still waiting")
	p.golden()
	close(b.podGate)
}

func TestGoldenSnapshots(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "vaultwarden")
	p.press("G", "k", "s")
	p.waitFor("5 snapshots", "custom format")
	p.press("enter")
	p.waitFor("rsa_key.pub.pem")
	p.golden()
}

func TestGoldenSnapshotsDatabaseTag(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "vaultwarden")
	p.press("G", "k", "s")
	p.waitFor("5 snapshots")
	p.press("tab")
	p.waitFor("2 snapshots", "plain SQL (old)")
	p.golden()
}

func TestGoldenRestoreTarget(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "vaultwarden")
	p.press("G", "k", "s")
	p.waitFor("5 snapshots")
	p.press("r")
	p.waitFor("Where should it go?", "Over the live volume", "Needs a db snapshot")
	p.golden()
}

func TestGoldenRestoreConfirm(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "vaultwarden")
	p.press("G", "k", "s")
	p.waitFor("5 snapshots")
	p.press("r")
	p.waitFor("Over the live volume")
	p.press("down", "down", "enter")
	p.waitFor("would change")
	p.typeText("rest")
	p.waitFor("[ rest")
	p.golden()
}

func TestGoldenDoctor(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "minecraft")
	p.press("j", "j", "j", "d")
	p.waitFor("1 failing", "→ l view log")
	p.golden()
}

func TestGoldenHelpWithoutPermissions(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.denied["patch scheduledbackups"] = true
	b.denied["create pods/exec"] = true
	b.denied["list pods"] = true
	p := runProgram(t, b, allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "wx")
	p.press("?")
	p.waitFor("KEYS ON THIS SCREEN", "NOT WATCHED")
	p.golden()
}

func TestGoldenCheckOutput(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, frameWidth, frameHeight)
	p.waitFor("6 backups", "apprise")
	p.press("c")
	p.waitFor("Check repository/restic")
	p.typeText("5%")
	p.press("enter")
	p.waitFor("no errors were found", "done")
	p.golden()
}

func TestGoldenTooSmall(t *testing.T) {
	t.Parallel()
	p := runProgram(t, newFakeBackend(t), allNamespaces, 50, 10)
	p.waitFor("needs a terminal at least")
	p.golden()
}
