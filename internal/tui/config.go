package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"sigs.k8s.io/yaml"
)

// Environment variables that override the config file.
const (
	EnvAllNamespaces = "B2BK_ALL_NAMESPACES"
	EnvMouse         = "B2BK_MOUSE"
)

// Environment variables that turn color off.
const (
	EnvNoColor       = "NO_COLOR"
	EnvCLIColor      = "CLICOLOR"
	EnvCLIColorForce = "CLICOLOR_FORCE"
)

// Options are the TUI's settings.
type Options struct {
	// AllNamespaces starts on every namespace instead of the current one.
	AllNamespaces bool
	// Mouse captures the mouse for clicks and the wheel.
	Mouse bool
	// NoColor draws without color, keeping bold, underline and reverse video.
	// Only the environment sets it; see NoColor.
	NoColor bool
}

// DefaultOptions are the settings with no flag, variable or file.
func DefaultOptions() Options { return Options{Mouse: true} }

// Flags are the command-line settings, nil when not given.
type Flags struct {
	AllNamespaces *bool
	Mouse         *bool
}

type configFile struct {
	TUI struct {
		AllNamespaces *bool `json:"allNamespaces,omitempty"`
		Mouse         *bool `json:"mouse,omitempty"`
	} `json:"tui"`
}

// ConfigPath is where the config file lives: $XDG_CONFIG_HOME/b2bk/config.yaml,
// else ~/.config/b2bk/config.yaml.
func ConfigPath(getenv func(string) string) (string, error) {
	if dir := getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "b2bk", "config.yaml"), nil
	}
	home := getenv("HOME")
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(home, ".config", "b2bk", "config.yaml"), nil
}

// LoadOptions resolves the settings: flags, then environment variables, then
// the config file, then the defaults.
func LoadOptions(flags Flags, getenv func(string) string) (Options, error) {
	opts := DefaultOptions()

	path, err := ConfigPath(getenv)
	if err != nil {
		return opts, err
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return opts, fmt.Errorf("reading %s: %w", path, err)
	default:
		var file configFile
		if err := yaml.UnmarshalStrict(data, &file); err != nil {
			return opts, fmt.Errorf("reading %s: %w", path, err)
		}
		set(&opts.AllNamespaces, file.TUI.AllNamespaces)
		set(&opts.Mouse, file.TUI.Mouse)
	}

	for name, into := range map[string]*bool{EnvAllNamespaces: &opts.AllNamespaces, EnvMouse: &opts.Mouse} {
		raw := getenv(name)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return opts, fmt.Errorf("%s=%q is not true or false", name, raw)
		}
		*into = v
	}

	set(&opts.AllNamespaces, flags.AllNamespaces)
	set(&opts.Mouse, flags.Mouse)
	opts.NoColor = NoColor(getenv)
	return opts, nil
}

// NoColor reports whether the environment asks for no color: NO_COLOR set to
// anything but the empty string (https://no-color.org), or CLICOLOR=0 without
// CLICOLOR_FORCE (https://bixense.com/clicolors). NO_COLOR wins over
// CLICOLOR_FORCE.
func NoColor(getenv func(string) string) bool {
	if getenv(EnvNoColor) != "" {
		return true
	}
	force := getenv(EnvCLIColorForce)
	return getenv(EnvCLIColor) == "0" && (force == "" || force == "0")
}

func set(into *bool, v *bool) {
	if v != nil {
		*into = *v
	}
}
