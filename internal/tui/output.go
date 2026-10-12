package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/types"
)

// outputScreen shows what a background b2bk command writes: check, compare.
type outputScreen struct {
	key    types.NamespacedName
	title  string
	stream *stream
	view   logView
}

func newOutputScreen(k types.NamespacedName, title string, s *stream) *outputScreen {
	return &outputScreen{key: k, title: title, stream: s, view: logView{follow: true}}
}

func (o *outputScreen) typing(*Model) bool { return o.view.searching }

func (o *outputScreen) crumbs(*Model) []string { return []string{o.key.Namespace, o.key.Name, o.title} }

func (o *outputScreen) headerRight(m *Model) string {
	return streamState(m, o.stream.snapshot()) + "     " + styleKey.Render(keyEsc) + " back"
}

func streamState(m *Model, sv streamView) string {
	switch {
	case !sv.done:
		return styleWarn.Render(symRunning + " running " + clock(m.now().Sub(sv.started)))
	case sv.err != nil:
		return styleFail.Render(symFail + " failed")
	}
	return styleOK.Render(symSucceeded + " done")
}

func (o *outputScreen) top(_ *Model, width int) []string {
	sv := o.stream.snapshot()
	if sv.progress == nil {
		return nil
	}
	return []string{progressBarLine(sv.label, sv.progress, width)}
}

func (o *outputScreen) body(_ *Model, width, height int) []string {
	return o.view.render(o.stream.snapshot(), time.Local, width, height)
}

func (o *outputScreen) strip(*Model, int) string {
	if o.view.searching {
		return " " + styleKey.Render("/") + " " + o.view.search.view(30) + "  " + styleDim.Render("⏎ keep  esc clear")
	}
	return ""
}

func (o *outputScreen) bindings(*Model) []binding {
	return []binding{
		{disp: "f", label: wordFollow},
		{disp: "/", label: "search"},
		{disp: "w", label: wordWrap},
		{disp: "g/G", keys: []string{"g"}, label: "top/end"},
		{keys: []string{keyEsc}, disp: keyEsc, label: wordBack},
	}
}

func (o *outputScreen) handleKey(_ *Model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	return nil, o.view.key(msg)
}

func (o *outputScreen) click(*Model, int, int) tea.Cmd { return nil }

func (o *outputScreen) scroll(_ *Model, delta int) { o.view.scroll(delta) }
