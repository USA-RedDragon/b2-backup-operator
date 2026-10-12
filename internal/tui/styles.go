package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

// The palette is the terminal's own 16 colors, so the TUI follows its theme.
var (
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
	styleOK       = lipgloss.NewStyle().Foreground(lipgloss.Green)
	styleWarn     = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	styleFail     = lipgloss.NewStyle().Foreground(lipgloss.Red)
	styleAccent   = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
	styleBold     = lipgloss.NewStyle().Bold(true)
	styleBadge    = lipgloss.NewStyle().Reverse(true).Bold(true)
	styleSelected = lipgloss.NewStyle().Bold(true)
	styleKey      = lipgloss.NewStyle().Bold(true)
	styleField    = lipgloss.NewStyle().Underline(true)
)

const (
	symOK          = "●"
	symRunning     = "◐"
	symPaused      = "⏸"
	symFail        = "✖"
	symWarn        = "!"
	symWaiting     = "○"
	symSucceeded   = "✔"
	symInfo        = "·"
	symCursor      = "›"
	symEllipsis    = "…"
	symChecked     = "◉"
	symUnchecked   = "○"
	symUnavailable = "-"
	symRule        = "─"
	symBar         = "█"
	symTrack       = "░"
	symRunBar      = "▄"
	symRunBarHalf  = "▖"
	symRunTrack    = "▁"
	symEm          = "—"
)

// fit truncates s to width cells and pads it to exactly width.
func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = ansi.Truncate(s, width, symEllipsis)
	if w := lipgloss.Width(s); w < width {
		s += strings.Repeat(" ", width-w)
	}
	return s
}

// trunc truncates s to width cells.
func trunc(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, symEllipsis)
}

// spread puts left and right at the two ends of a line width cells wide,
// truncating left when they do not fit.
func spread(left, right string, width int) string {
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if lw+rw+1 > width {
		if rw+1 >= width {
			return trunc(left, width)
		}
		left = trunc(left, width-rw-1)
		lw = lipgloss.Width(left)
	}
	return left + strings.Repeat(" ", max(1, width-lw-rw)) + right
}

func rule(width int) string { return styleDim.Render(strings.Repeat(symRule, max(0, width))) }

// humanBytes renders a size the way restic does.
func humanBytes(n uint64) string {
	return backup.FormatBytes(int64(min(n, math.MaxInt64)))
}

// clock renders a short duration as running time: 24s, 2m14s, 1h02m.
func clock(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// span renders how far away a time is: 57m, 3h01m, 2d04h.
func span(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// ago renders how long ago t was: 40s ago, 17h ago, 3d ago.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// commas renders n with thousands separators.
func commas(n int) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// lines pads or cuts a block to exactly height lines.
func lines(block []string, height int) []string {
	if len(block) > height {
		return block[:height]
	}
	for len(block) < height {
		block = append(block, "")
	}
	return block
}

// wrapped breaks text into lines width cells wide, each starting with indent.
func wrapped(text string, width int, indent string, style lipgloss.Style) []string {
	var out []string
	for line := range strings.SplitSeq(ansi.Wordwrap(text, max(10, width-len(indent)), " "), "\n") {
		out = append(out, style.Render(indent+line))
	}
	return out
}
