package tui

import (
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// field is a one-line text input. It draws its own cursor, so a frame does not
// depend on a blinking one.
type field struct {
	value   string
	focused bool
}

// edit applies a key to the field and reports whether it used it.
func (f *field) edit(msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "backspace", "ctrl+h":
		if f.value != "" {
			_, size := utf8.DecodeLastRuneInString(f.value)
			f.value = f.value[:len(f.value)-size]
		}
		return true
	case "ctrl+u":
		f.value = ""
		return true
	case "ctrl+w":
		trimmed := strings.TrimRight(f.value, " /")
		if i := strings.LastIndexAny(trimmed, " /"); i >= 0 {
			f.value = trimmed[:i+1]
		} else {
			f.value = ""
		}
		return true
	}
	if text := msg.Key().Text; text != "" && !strings.ContainsAny(text, "\r\n\t") {
		f.value += text
		return true
	}
	return false
}

// view draws the field as [ value█    ], width cells inside the brackets.
func (f *field) view(width int) string {
	text := f.value
	if f.focused {
		text += symBar
	}
	width = max(1, width)
	if w := lipgloss.Width(text); w > width {
		text = symEllipsis + ansi.TruncateLeft(text, w-width+1, "")
	}
	inner := fit(text, width)
	if f.focused {
		return "[ " + styleField.Render(inner) + " ]"
	}
	return "[ " + inner + " ]"
}
