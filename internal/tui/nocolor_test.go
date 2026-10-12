package tui

import (
	"bytes"
	"io"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"
)

// colorTerminal is a 256-color terminal; TTY_FORCE makes colorprofile treat
// the test's buffer as one.
var colorTerminal = []string{"TERM=xterm-256color", "TTY_FORCE=1"}

const (
	capRGB = "RGB"
	capTc  = "Tc"
)

var sgr = regexp.MustCompile(`\x1b\[([0-9;:]*)m`)

// sgrAttrs splits every SGR sequence in b into the color ones and the set of
// other attributes it turns on.
func sgrAttrs(b []byte) (colors []string, attrs map[int]bool) {
	attrs = map[int]bool{}
	for _, m := range sgr.FindAllSubmatch(b, -1) {
		params := strings.FieldsFunc(string(m[1]), func(r rune) bool { return r == ';' || r == ':' })
		color := false
		for i := 0; i < len(params); i++ {
			n, _ := strconv.Atoi(params[i])
			switch {
			case n == 38 || n == 48 || n == 58:
				color = true
				if i+1 < len(params) && params[i+1] == "5" {
					i += 2
				} else if i+1 < len(params) && params[i+1] == "2" {
					i += 4
				}
			case n >= 30 && n <= 37, n == 39, n >= 40 && n <= 47, n == 49, n == 59,
				n >= 90 && n <= 97, n >= 100 && n <= 107:
				color = true
			default:
				attrs[n] = true
			}
		}
		if color {
			colors = append(colors, strconv.Quote(string(m[0])))
		}
	}
	return colors, attrs
}

func TestNoColorFromEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{EnvNoColor: ""}, false},
		{map[string]string{EnvNoColor: "1"}, true},
		{map[string]string{EnvNoColor: "0"}, true},
		{map[string]string{EnvNoColor: strconv.FormatBool(false)}, true},
		{map[string]string{EnvNoColor: "yes"}, true},
		{map[string]string{EnvNoColor: "1", EnvCLIColorForce: "1"}, true},
		{map[string]string{EnvCLIColor: "0"}, true},
		{map[string]string{EnvCLIColor: "1"}, false},
		{map[string]string{EnvCLIColor: "0", EnvCLIColorForce: "1"}, false},
		{map[string]string{EnvCLIColor: "0", EnvCLIColorForce: "0"}, true},
		{map[string]string{EnvCLIColorForce: "1"}, false},
	} {
		env := map[string]string{"HOME": t.TempDir()}
		maps.Copy(env, tc.env)
		getenv := func(k string) string { return env[k] }
		if got := NoColor(getenv); got != tc.want {
			t.Errorf("NoColor(%v) = %v, want %v", tc.env, got, tc.want)
		}
		opts, err := LoadOptions(Flags{}, getenv)
		if err != nil {
			t.Fatal(err)
		}
		if opts.NoColor != tc.want {
			t.Errorf("LoadOptions(%v).NoColor = %v, want %v", tc.env, opts.NoColor, tc.want)
		}
	}
}

func TestProgramProfile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		environ []string
		noColor bool
		want    colorprofile.Profile
	}{
		{colorTerminal, false, colorprofile.ANSI256},
		{colorTerminal, true, colorprofile.ASCII},
		{append([]string{"COLORTERM=truecolor"}, colorTerminal...), true, colorprofile.ASCII},
		{[]string{"TERM=xterm-256color", "TTY_FORCE=1", EnvNoColor + "=1"}, false, colorprofile.ASCII},
		{[]string{"TERM=dumb"}, true, colorprofile.NoTTY},
	} {
		profile := programProfile(Options{NoColor: tc.noColor}, io.Discard, tc.environ)
		if profile != tc.want {
			t.Errorf("profile in %v with NoColor %v = %v, want %v", tc.environ, tc.noColor, profile, tc.want)
		}
	}
}

func TestKeepProfile(t *testing.T) {
	t.Parallel()
	if msg := keepProfile(colorprofile.ASCII, tea.CapabilityMsg{Content: capRGB}); msg != nil {
		t.Errorf("an RGB capability passed under ASCII: %#v", msg)
	}
	if msg := keepProfile(colorprofile.ASCII, tea.CapabilityMsg{Content: capTc}); msg != nil {
		t.Errorf("a Tc capability passed under ASCII: %#v", msg)
	}
	if msg := keepProfile(colorprofile.TrueColor, tea.CapabilityMsg{Content: capRGB}); msg == nil {
		t.Error("an RGB capability was dropped under TrueColor")
	}
	if msg := keepProfile(colorprofile.ASCII, tea.ColorProfileMsg{Profile: colorprofile.TrueColor}); msg !=
		(tea.ColorProfileMsg{Profile: colorprofile.ASCII}) {
		t.Errorf("a TrueColor profile message became %#v under ASCII", msg)
	}
}

// stream quits and returns every byte the program wrote, and its last frame
// as an ASCII-profile terminal is sent it.
func (p *program) stream() (raw []byte, frame string) {
	p.t.Helper()
	if err := p.tm.Quit(); err != nil {
		p.t.Fatal(err)
	}
	m := p.tm.FinalModel(p.t, teatest.WithFinalTimeout(10*time.Second)).(*Model)
	raw, err := io.ReadAll(p.tm.FinalOutput(p.t))
	if err != nil {
		p.t.Fatal(err)
	}
	var b bytes.Buffer
	w := &colorprofile.Writer{Forward: &b, Profile: colorprofile.ASCII}
	if _, err := w.Write([]byte(m.render())); err != nil {
		p.t.Fatal(err)
	}
	return raw, b.String()
}

// noColorFrame checks that the program wrote no color and still marked what
// matters with bold, reverse video or underline, then compares the last frame,
// with its remaining SGR sequences, with testdata/<test>.golden.
func noColorFrame(t *testing.T, p *program, attrs ...int) {
	t.Helper()
	raw, frame := p.stream()
	colors, got := sgrAttrs(raw)
	if len(colors) > 0 {
		t.Errorf("wrote %d color sequences under NO_COLOR, such as %s", len(colors), colors[0])
	}
	if c, _ := sgrAttrs([]byte(frame)); len(c) > 0 {
		t.Errorf("the frame has color under NO_COLOR: %s", c[0])
	}
	for _, a := range attrs {
		if !got[a] {
			t.Errorf("SGR %d never appeared; attributes seen: %v", a, got)
		}
	}
	golden.RequireEqual(t, frame)
}

const (
	sgrBold      = 1
	sgrUnderline = 4
	sgrReverse   = 7
)

var noColor = Options{AllNamespaces: true, Mouse: true, NoColor: true}

func TestColorWithoutNoColor(t *testing.T) {
	t.Parallel()
	p := runProgramIn(t, newFakeBackend(t), allNamespaces, colorTerminal, frameWidth, frameHeight)
	p.waitFor("6 backups", "wx", "B2 us-east-005")
	raw, _ := p.stream()
	if colors, _ := sgrAttrs(raw); len(colors) == 0 {
		t.Fatal("no color on a 256-color terminal without NO_COLOR, so the NO_COLOR tests prove nothing")
	}
}

func TestGoldenNoColorDashboard(t *testing.T) {
	t.Parallel()
	p := runProgramIn(t, newFakeBackend(t), noColor, colorTerminal, frameWidth, frameHeight)
	p.waitFor("6 backups", "wx", "B2 us-east-005")
	p.press("j")
	p.waitFor("› gitea")
	for _, want := range []string{"● ok", "◐ running", "⏸ suspended", "✖ failed"} {
		p.waitFor(want)
	}
	noColorFrame(t, p, sgrBold, sgrReverse)
}

func TestGoldenNoColorTerminalAnswersRGB(t *testing.T) {
	t.Parallel()
	p := runProgramIn(t, newFakeBackend(t), noColor, colorTerminal, frameWidth, frameHeight)
	p.waitFor("6 backups", "wx")
	p.tm.Send(tea.CapabilityMsg{Content: capRGB})
	p.tm.Send(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	p.press("j", "j")
	p.waitFor("› kosync")
	noColorFrame(t, p, sgrBold, sgrReverse)
}

func TestGoldenNoColorLogs(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.logs[nsGitea+"/restic-manual-x8k2qp"] += "2026-10-11T23:49:21Z \x1b[31;48;5;22mcolored by the app\x1b[0m\n"
	p := runProgramIn(t, b, noColor, colorTerminal, frameWidth, frameHeight)
	p.waitFor("6 backups", "gitea")
	p.press("j", "l")
	p.waitFor("61%", "file changed during read", "colored by the app", symBar, symTrack)
	noColorFrame(t, p, sgrBold, sgrReverse)
}

func TestGoldenNoColorRestoreTarget(t *testing.T) {
	t.Parallel()
	p := runProgramIn(t, newFakeBackend(t), noColor, colorTerminal, frameWidth, frameHeight)
	p.waitFor("6 backups", "vaultwarden")
	p.press("G", "k", "s")
	p.waitFor("5 snapshots")
	p.press("r")
	p.waitFor("› ◉ A new claim", "- Into the database           unavailable: needs a db snapshot")
	p.press("tab")
	p.waitFor("[ vaultwarden-restore-1011█")
	noColorFrame(t, p, sgrBold, sgrReverse, sgrUnderline)
}

func TestGoldenNoColorDoctor(t *testing.T) {
	t.Parallel()
	p := runProgramIn(t, newFakeBackend(t), noColor, colorTerminal, frameWidth, frameHeight)
	p.waitFor("6 backups", "minecraft")
	p.press("j", "j", "j", "d")
	p.waitFor("1 failing", "→ l view log", symFail, symSucceeded)
	noColorFrame(t, p, sgrBold, sgrReverse)
}

func TestGoldenNoColorHelpWithoutPermissions(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.denied["patch scheduledbackups"] = true
	p := runProgramIn(t, b, noColor, colorTerminal, frameWidth, frameHeight)
	p.waitFor("6 backups", "wx")
	p.press("?")
	p.waitFor("KEYS ON THIS SCREEN", "backup now *", "UNAVAILABLE *")
	noColorFrame(t, p, sgrBold, sgrReverse)
}
