package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	minWidth  = 60
	minHeight = 12

	flashFor      = 8 * time.Second
	refreshDelay  = 150 * time.Millisecond
	clockInterval = time.Second
)

// binding is a key a screen offers.
type binding struct {
	keys  []string
	disp  string
	label string
	act   action
	// helpOnly keeps it out of the footer; ? still lists it.
	helpOnly bool
}

// screen is one view of the TUI. The root model owns the frame around it.
type screen interface {
	crumbs(m *Model) []string
	headerRight(m *Model) string
	top(m *Model, width int) []string
	body(m *Model, width, height int) []string
	strip(m *Model, width int) string
	bindings(m *Model) []binding
	handleKey(m *Model, msg tea.KeyPressMsg) (tea.Cmd, bool)
	click(m *Model, x, y int) tea.Cmd
	scroll(m *Model, delta int)
	typing(m *Model) bool
}

// prompt asks something on the status line: one key, or a line of text.
type prompt struct {
	text    string
	input   *field
	onKey   func(key string) tea.Cmd
	onEnter func(value string) tea.Cmd
}

// Model is the root of the TUI. It owns the watches, the session pods and the
// frame, and hands keys and clicks to the screen on top.
type Model struct {
	ctx     context.Context
	cancel  context.CancelFunc
	backend Backend
	now     func() time.Time
	hub     *hub

	width, height int

	allNamespaces bool
	namespace     string
	kubeContext   string
	mouse         bool

	access   access
	operator OperatorInfo

	state     *clusterState
	stateErr  error
	watchErr  error
	watchGen  int
	stopWatch context.CancelFunc
	changes   chan struct{}
	reader    client.Reader
	kinds     []Kind

	screens []screen

	problemsFirst bool
	filter        field
	filtering     bool

	help   bool
	prompt *prompt

	flash      string
	flashStyle lipgloss.Style
	flashAt    time.Time

	sessions *sessions
	streams  map[string]*stream
	// triggered is when this TUI triggered each backup, so its run can be
	// told apart as started by you.
	triggered map[types.NamespacedName]time.Time
	lastLines map[string]string
	runs      []*stream
}

// New returns the root model.
func New(ctx context.Context, b Backend, opts Options) *Model {
	ctx, cancel := context.WithCancel(ctx)
	m := &Model{
		ctx: ctx, cancel: cancel, backend: b, now: time.Now, hub: newHub(),
		allNamespaces: opts.AllNamespaces, mouse: opts.Mouse,
		access:  allowAll(),
		streams: map[string]*stream{}, triggered: map[types.NamespacedName]time.Time{},
		lastLines: map[string]string{},
		width:     104, height: 30,
	}
	m.kubeContext = b.Context()
	if ns, err := b.Namespace(); err == nil {
		m.namespace = ns
	} else {
		m.stateErr = err
	}
	m.sessions = newSessions(ctx, b, m.hub, func() time.Time { return m.now() })
	m.screens = []screen{newDashboard()}
	return m
}

// Close stops every watch and stream, waits for what they started to clean up
// after itself, and removes every pod the TUI started.
func (m *Model) Close() {
	m.cancel()
	if m.stopWatch != nil {
		m.stopWatch()
	}
	for _, s := range m.streams {
		s.stop()
	}
	for _, s := range m.runs {
		s.stop()
	}
	m.hub.wg.Wait()
	m.sessions.closeAll()
}

func (m *Model) scope() string {
	if m.allNamespaces {
		return ""
	}
	return m.namespace
}

type accessMsg struct {
	scope  string
	access access
}

type operatorMsg OperatorInfo

type watchMsg struct {
	gen     int
	reader  client.Reader
	changes chan struct{}
	err     error
}

type changedMsg struct{ gen int }

type stateMsg struct {
	gen   int
	state *clusterState
	err   error
}

type tickMsg time.Time

type doneMsg struct {
	text string
	err  error
}

type lastLineMsg struct{ job, line string }

// Init starts the access review, which starts the watch.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.reviewAccess(), m.readOperator(), m.hub.wait(m.ctx), m.tick())
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(clockInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) reviewAccess() tea.Cmd {
	scope := m.scope()
	b := m.backend
	ctx := m.ctx
	return func() tea.Msg {
		cs, err := b.Clientset()
		if err != nil {
			return accessMsg{scope: scope, access: allowAll()}
		}
		return accessMsg{scope: scope, access: reviewAccess(ctx, cs, scope)}
	}
}

func (m *Model) readOperator() tea.Cmd {
	b, ctx := m.backend, m.ctx
	return func() tea.Msg { return operatorMsg(b.Operator(ctx)) }
}

func (m *Model) startWatch() tea.Cmd {
	if m.stopWatch != nil {
		m.stopWatch()
	}
	m.watchGen++
	gen, scope, kinds := m.watchGen, m.scope(), m.access.watched()
	m.kinds = kinds
	ctx, cancel := context.WithCancel(m.ctx)
	m.stopWatch = cancel
	m.state, m.reader, m.watchErr = nil, nil, nil
	b := m.backend
	return func() tea.Msg {
		changes := make(chan struct{}, 1)
		notify := func() {
			select {
			case changes <- struct{}{}:
			default:
			}
		}
		rd, err := b.Watch(ctx, scope, kinds, notify)
		return watchMsg{gen: gen, reader: rd, changes: changes, err: err}
	}
}

func (m *Model) waitChange(gen int, changes chan struct{}) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		select {
		case <-changes:
			select {
			case <-time.After(refreshDelay):
			case <-ctx.Done():
				return nil
			}
			return changedMsg{gen: gen}
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Model) load(gen int) tea.Cmd {
	rd, scope, kinds, ctx := m.reader, m.scope(), m.kinds, m.ctx
	if rd == nil {
		return nil
	}
	return func() tea.Msg {
		st, err := buildState(ctx, rd, scope, kinds)
		return stateMsg{gen: gen, state: st, err: err}
	}
}

// Update routes a message to the root or the screen on top.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case accessMsg:
		if msg.scope != m.scope() {
			return m, nil
		}
		m.access = msg.access
		return m, m.startWatch()
	case operatorMsg:
		m.operator = OperatorInfo(msg)
		return m, nil
	case watchMsg:
		if msg.gen != m.watchGen {
			return m, nil
		}
		if msg.err != nil {
			m.watchErr = msg.err
			return m, nil
		}
		m.reader, m.changes = msg.reader, msg.changes
		return m, tea.Batch(m.load(msg.gen), m.waitChange(msg.gen, msg.changes))
	case changedMsg:
		if msg.gen != m.watchGen {
			return m, nil
		}
		return m, tea.Batch(m.load(msg.gen), m.waitChange(msg.gen, m.changes))
	case stateMsg:
		if msg.gen != m.watchGen {
			return m, nil
		}
		if msg.err != nil {
			m.stateErr = msg.err
			return m, nil
		}
		m.state, m.stateErr = msg.state, nil
		m.followMine()
		return m, nil
	case eventMsg:
		return m, m.hub.wait(m.ctx)
	case tickMsg:
		return m, m.tick()
	case doneMsg:
		if msg.err != nil {
			m.setFlash(msg.err.Error(), styleFail)
		} else if msg.text != "" {
			m.setFlash(msg.text, styleOK)
		}
		return m, nil
	case lastLineMsg:
		m.lastLines[msg.job] = msg.line
		return m, nil
	case tea.KeyPressMsg:
		return m, m.onKey(msg)
	case tea.MouseClickMsg:
		return m, m.onClick(msg.Mouse())
	case tea.MouseWheelMsg:
		m.onWheel(msg.Mouse())
		return m, nil
	}
	if r, ok := msg.(interface{ route() }); ok {
		r.route()
	}
	return m, nil
}

func (m *Model) top() screen { return m.screens[len(m.screens)-1] }

func (m *Model) push(s screen) { m.screens = append(m.screens, s) }

func (m *Model) pop() {
	if len(m.screens) > 1 {
		if c, ok := m.top().(interface{ closeScreen() }); ok {
			c.closeScreen()
		}
		m.screens = m.screens[:len(m.screens)-1]
	}
}

func (m *Model) setFlash(text string, style lipgloss.Style) {
	m.flash, m.flashStyle, m.flashAt = text, style, m.now()
}

// quit leaves the TUI, after asking when a restore is still writing.
func (m *Model) quit() tea.Cmd {
	for _, s := range m.screens {
		if w, ok := s.(*restoreScreen); ok && w.running() {
			m.ask("A restore is running. Quitting stops it, and b2bk keeps what it was writing held as partial."+
				"  q quits anyway  any other key stays", func(k string) tea.Cmd {
				if k == "q" {
					return tea.Quit
				}
				return nil
			})
			return nil
		}
	}
	return tea.Quit
}

func (m *Model) onKey(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		if m.prompt != nil {
			return tea.Quit
		}
		return m.quit()
	}
	if p := m.prompt; p != nil {
		return m.onPromptKey(p, msg)
	}
	if m.help {
		m.help = false
		return nil
	}
	s := m.top()
	if !s.typing(m) {
		switch k {
		case "q":
			return m.quit()
		case "?":
			m.help = true
			return nil
		case "m":
			m.mouse = !m.mouse
			if m.mouse {
				m.setFlash("mouse on: click to select, wheel to scroll; m turns it off to select text", styleDim)
			} else {
				m.setFlash("mouse off: the terminal selects text again; m turns it back on", styleDim)
			}
			return nil
		}
	}
	if cmd, ok := s.handleKey(m, msg); ok {
		return cmd
	}
	if k == keyEsc {
		m.pop()
	}
	return nil
}

func (m *Model) onPromptKey(p *prompt, msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if p.input == nil {
		m.prompt = nil
		if p.onKey != nil {
			return p.onKey(k)
		}
		return nil
	}
	switch k {
	case keyEsc:
		m.prompt = nil
		return nil
	case keyEnter:
		m.prompt = nil
		return p.onEnter(p.input.value)
	}
	p.input.edit(msg)
	return nil
}

func (m *Model) ask(text string, onKey func(key string) tea.Cmd) {
	m.prompt = &prompt{text: text, onKey: onKey}
}

func (m *Model) askText(text, value string, onEnter func(string) tea.Cmd) {
	m.prompt = &prompt{text: text, input: &field{value: value, focused: true}, onEnter: onEnter}
}

// frame is where each part of the screen is drawn.
type frame struct {
	head    []string
	bodyTop int
	bodyH   int
	strip   string
	footY   int
}

func (m *Model) frame() frame {
	s := m.top()
	f := frame{head: []string{m.header(s), rule(m.width)}}
	if top := s.top(m, m.width); len(top) > 0 {
		f.head = append(f.head, top...)
		f.head = append(f.head, rule(m.width))
	}
	f.strip = m.stripText(s)
	foot := 2
	if f.strip != "" {
		foot = 4
	}
	f.bodyTop = len(f.head)
	f.bodyH = max(0, m.height-len(f.head)-foot)
	f.footY = m.height - 1
	return f
}

// View draws the frame and the screen on top.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "b2bk"
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func (m *Model) render() string {
	if m.width < minWidth || m.height < minHeight {
		return fmt.Sprintf("b2bk needs a terminal at least %d×%d; this one is %d×%d. q quits.",
			minWidth, minHeight, m.width, m.height)
	}
	s := m.top()
	f := m.frame()
	out := slices.Clone(f.head)
	var body []string
	if m.help {
		body = m.helpBody(s, f.bodyH)
	} else {
		body = s.body(m, m.width, f.bodyH)
	}
	for _, line := range lines(body, f.bodyH) {
		out = append(out, trunc(line, m.width))
	}
	out = append(out, rule(m.width))
	if f.strip != "" {
		out = append(out, trunc(f.strip, m.width), rule(m.width))
	}
	out = append(out, m.footer(s))
	return strings.Join(out, "\n")
}

func (m *Model) header(s screen) string {
	left := " " + styleBadge.Render(" b2bk ") + " "
	if h, ok := s.(interface{ headerLeft(m *Model) string }); ok {
		left += " " + h.headerLeft(m)
	}
	if crumbs := s.crumbs(m); len(crumbs) > 0 {
		left += " " + symCursor + " " + strings.Join(crumbs, " "+symCursor+" ")
	}
	right := s.headerRight(m) + " "
	lw := lipgloss.Width(left)
	if lw+lipgloss.Width(right)+2 > m.width {
		right = trunc(right, max(0, m.width-lw-2))
	}
	return spread(left, right, m.width)
}

func (m *Model) stripText(s screen) string {
	switch {
	case m.prompt != nil && m.prompt.input != nil:
		return " " + m.prompt.text + "  " + m.prompt.input.view(24) + "  " + styleDim.Render("⏎ ok  esc cancel")
	case m.prompt != nil:
		return " " + styleWarn.Render(m.prompt.text)
	case m.flash != "" && m.now().Sub(m.flashAt) < flashFor:
		return " " + m.flashStyle.Render(oneLine(m.flash))
	}
	return s.strip(m, m.width)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func (m *Model) footer(s screen) string {
	var parts []string
	for _, b := range s.bindings(m) {
		if b.helpOnly || !m.access.can(b.act) {
			continue
		}
		parts = append(parts, styleKey.Render(b.disp)+" "+b.label)
	}
	return trunc(" "+strings.Join(parts, "  "), m.width)
}

// footerHit finds the binding drawn at column x of the footer.
func (m *Model) footerHit(s screen, x int) *binding {
	col := 1
	for _, b := range s.bindings(m) {
		if b.helpOnly || !m.access.can(b.act) {
			continue
		}
		w := lipgloss.Width(b.disp) + 1 + lipgloss.Width(b.label)
		if x >= col && x < col+w {
			return &b
		}
		col += w + 2
	}
	return nil
}

func (m *Model) globalBindings() []binding {
	mouse := "mouse off, so the terminal selects text"
	if !m.mouse {
		mouse = "mouse on, for clicks and the wheel"
	}
	return []binding{
		{disp: "?", label: "show every key for this screen"},
		{disp: "m", label: mouse},
		{disp: keyEsc, label: "back one screen"},
		{disp: "q", label: "quit, also ctrl+c; removes its pods"},
	}
}

func (m *Model) helpBody(s screen, height int) []string {
	col := max(30, m.width/2)
	keyLine := func(b binding) string {
		line := fit(styleKey.Render(b.disp), 10) + b.label
		if b.act != "" && !m.access.can(b.act) {
			line = styleDim.Render(fit(b.disp, 10) + b.label + " *")
		}
		return line
	}
	bindings := s.bindings(m)
	left := make([]string, 0, len(bindings)+1)
	left = append(left, styleBold.Render("KEYS ON THIS SCREEN"))
	var why []string
	reasons := map[string][]string{}
	for _, b := range bindings {
		left = append(left, keyLine(b))
		if r := m.access.denied[b.act]; b.act != "" && r != "" {
			if _, ok := reasons[r]; !ok {
				why = append(why, r)
			}
			reasons[r] = append(reasons[r], b.disp)
		}
	}
	right := []string{styleBold.Render("EVERYWHERE")}
	for _, b := range m.globalBindings() {
		right = append(right, keyLine(b))
	}
	wrap := func(text string) []string {
		return strings.Split(ansi.Wordwrap(text, max(10, m.width-col-3), " "), "\n")
	}
	if len(why) > 0 {
		right = append(right, "", styleBold.Render("UNAVAILABLE *"))
		for _, r := range why {
			for _, line := range wrap(strings.Join(reasons[r], " ") + ": " + r) {
				right = append(right, styleDim.Render(line))
			}
		}
	}
	if len(m.access.unwatched) > 0 {
		right = append(right, "", styleBold.Render("NOT WATCHED"))
		for _, k := range []Kind{KindScheduledBackup, KindRepository, KindCronJob, KindJob, KindPod} {
			if r := m.access.unwatched[k]; r != "" {
				for _, line := range wrap(r + ", so what depends on them is not shown") {
					right = append(right, styleDim.Render(line))
				}
			}
		}
	}
	rows := max(len(left), len(right))
	out := make([]string, 0, rows+3)
	out = append(out, "")
	for i := range rows {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		out = append(out, "  "+fit(l, col-2)+trunc(r, m.width-col-1))
	}
	out = append(out, "", styleDim.Render("  any key closes this"))
	return lines(out, height)
}

func (m *Model) onClick(mouse tea.Mouse) tea.Cmd {
	if m.help {
		m.help = false
		return nil
	}
	if m.prompt != nil {
		return nil
	}
	s := m.top()
	f := m.frame()
	if mouse.Y == f.footY {
		if b := m.footerHit(s, mouse.X); b != nil {
			return m.onKey(keyFor(b))
		}
		return nil
	}
	if mouse.Y >= f.bodyTop && mouse.Y < f.bodyTop+f.bodyH {
		return s.click(m, mouse.X, mouse.Y-f.bodyTop)
	}
	return nil
}

func (m *Model) onWheel(mouse tea.Mouse) {
	switch mouse.Button {
	case tea.MouseWheelUp:
		m.top().scroll(m, -3)
	case tea.MouseWheelDown:
		m.top().scroll(m, 3)
	}
}

// keyFor is the key press a footer binding stands for.
func keyFor(b *binding) tea.KeyPressMsg {
	k := b.disp
	if len(b.keys) > 0 {
		k = b.keys[0]
	}
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
	case keyRight:
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case keyLeft:
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	}
	r := []rune(k)
	return tea.KeyPressMsg{Code: r[0], Text: k}
}

// rowFor is the current row of a backup, or nil when it is gone.
func (m *Model) rowFor(k types.NamespacedName) *row {
	if m.state == nil {
		return nil
	}
	for _, r := range m.state.rows {
		if r.key() == k {
			return r
		}
	}
	return nil
}

// mine reports whether this TUI triggered the backup's Job.
func (m *Model) mine(r *row, job string) bool {
	at, ok := m.triggered[r.key()]
	trig := r.sb.Status.LastTriggerTime
	return ok && trig != nil && r.sb.Status.LastTriggerJob == job && !trig.Time.Before(at.Add(-time.Second))
}

// followMine follows the logs of every running backup this TUI triggered, so
// the dashboard can show its progress.
func (m *Model) followMine() {
	if m.state == nil {
		return
	}
	for _, r := range m.state.rows {
		if job := r.running(); job != nil && m.mine(r, job.Name) {
			m.logStream(r.sb.Namespace, job.Name, true)
		}
	}
}

// logStream is the stream of a Job's logs, started once per session.
func (m *Model) logStream(namespace, job string, follow bool) {
	id := namespace + "/" + job
	if s, ok := m.streams[id]; ok {
		v := s.snapshot()
		if !v.done || v.err == nil || !m.access.can(actLogs) {
			return
		}
	}
	b := m.backend
	s := startStream(m.ctx, m.hub, m.now(), func(ctx context.Context, w io.Writer) error {
		return b.Logs(ctx, namespace, job, follow, w)
	})
	m.streams[id] = s
}

// run starts a background action whose output is shown as it arrives.
func (m *Model) run(produce func(context.Context, io.Writer) error) *stream {
	s := startStream(m.ctx, m.hub, m.now(), produce)
	m.runs = append(m.runs, s)
	return s
}

func (m *Model) do(fn func(ctx context.Context) (string, error)) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		text, err := fn(ctx)
		return doneMsg{text: text, err: err}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return summary(err.Error())
}

var errGone = errors.New("this backup is gone")
