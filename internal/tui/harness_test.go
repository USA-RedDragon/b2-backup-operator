package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/apimachinery/pkg/types"
)

const (
	frameWidth  = 104
	frameHeight = 30
	cmdTimeout  = 200 * time.Millisecond
)

// newTestModel is a Model on the fake backend at the fixed clock.
func newTestModel(t *testing.T, b *fakeBackend, opts Options) *Model {
	t.Helper()
	m := New(t.Context(), b, opts)
	m.now = func() time.Time { return testNow }
	m.width, m.height = frameWidth, frameHeight
	t.Cleanup(m.Close)
	return m
}

// settle runs cmd and every command its messages lead to, feeding each message
// to the model, and drops a command that blocks, as a tick or a wait does.
func settle(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 500 {
			t.Fatal("the model never settled")
		}
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- next() }()
		var msg tea.Msg
		select {
		case msg = <-done:
		case <-time.After(cmdTimeout):
			continue
		}
		switch msg := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tickMsg, eventMsg:
		default:
			_, c := m.Update(msg)
			queue = append(queue, c)
		}
	}
}

// start brings the model up as the program would.
func start(t *testing.T, m *Model) {
	t.Helper()
	settle(t, m, m.Init())
}

// press sends keys, settling after each.
func press(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		_, cmd := m.Update(keyMsg(k))
		settle(t, m, cmd)
	}
}

// typeText types each character of s.
func typeText(t *testing.T, m *Model, s string) {
	t.Helper()
	for _, r := range s {
		_, cmd := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		settle(t, m, cmd)
	}
}

func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case keyEnter:
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case keyEsc:
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case keyTab:
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case keySpace:
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case keyUp:
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case keyDown:
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case keyLeft:
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case keyRight:
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case keyPgDown:
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// screenText is the screen as plain text.
func screenText(m *Model) string { return ansi.Strip(m.render()) }

// eventually polls cond until it holds, for background work.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func key(ns string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: nameRestic}
}
