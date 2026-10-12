// Package tui is the full-screen terminal dashboard a bare b2bk opens. Every
// action it offers goes through a Backend, which b2bk implements with the same
// functions its subcommands run, so the guards are the same.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Backend is what the TUI needs from b2bk: reads, watches and the actions of
// its subcommands.
type Backend interface {
	// Context is the kubeconfig context in use.
	Context() string
	// Namespace is the namespace from the kubeconfig or -n.
	Namespace() (string, error)
	// Reader reads straight from the API server.
	Reader() (client.Reader, error)
	// Clientset is used for SelfSubjectAccessReviews.
	Clientset() (kubernetes.Interface, error)
	// Watch starts informers on the operator's objects in namespace, or in
	// every namespace when it is empty, and calls changed after each event.
	// It returns a reader over their caches once they have synced.
	Watch(ctx context.Context, namespace string, kinds []Kind, changed func()) (client.Reader, error)
	// Operator describes the operator Deployment, when it can be read.
	Operator(ctx context.Context) OperatorInfo

	// BackupNow runs b2bk backup.
	BackupNow(ctx context.Context, namespace, name string) (string, error)
	// SetSuspend runs b2bk suspend or b2bk resume.
	SetSuspend(ctx context.Context, namespace, name string, suspend bool) (string, error)
	// Cancel runs b2bk cancel.
	Cancel(ctx context.Context, namespace, name string) (string, error)
	// Doctor runs the checks of b2bk doctor.
	Doctor(ctx context.Context, namespace, name string) ([]Check, error)
	// Logs streams a Job's logs as b2bk logs does.
	Logs(ctx context.Context, namespace, job string, follow bool, w io.Writer) error
	// LastLogLine is the last line a finished Job's pod logged, if it can be read.
	LastLogLine(ctx context.Context, namespace, job string) string
	// Check runs b2bk check, with --read-data-subset when readData is set.
	Check(ctx context.Context, namespace, name, readData string, w io.Writer) error

	// OpenPod starts the one-off pod b2bk snapshots would, and keeps it until
	// it is closed.
	OpenPod(ctx context.Context, namespace, name string, progress io.Writer) (Pod, error)
	// Snapshots lists the backup's snapshots, under every one of its source tags.
	Snapshots(ctx context.Context, pod Pod, namespace, name string) ([]Snapshot, error)
	// Files lists every file in a snapshot.
	Files(ctx context.Context, pod Pod, snapshot string) ([]Node, error)
	// Diff writes restic diff between two snapshots.
	Diff(ctx context.Context, pod Pod, from, to string, w io.Writer) error
	// Download copies a path out of a snapshot into a local directory, as b2bk
	// restore --to does.
	Download(ctx context.Context, pod Pod, req DownloadRequest, w io.Writer) error

	// RestoreTargets says why each restore target is unavailable, if it is.
	RestoreTargets(ctx context.Context, namespace, name, tag string) map[Target]string
	// Restore runs b2bk restore. An overwriting restore goes ahead only when
	// req.Confirm is the name b2bk restore asks to be typed.
	Restore(ctx context.Context, req RestoreRequest, w io.Writer) error
}

// Kind is an object kind the TUI watches.
type Kind string

// The kinds the TUI watches.
const (
	KindScheduledBackup Kind = "scheduledbackups"
	KindRepository      Kind = "repositories"
	KindCronJob         Kind = "cronjobs"
	KindJob             Kind = "jobs"
	KindPod             Kind = "pods"
)

// Pod is a running one-off restic pod.
type Pod interface {
	Name() string
	Exec(ctx context.Context, argv []string, stdout io.Writer) error
	Close()
}

// OperatorInfo describes the operator's Deployment.
type OperatorInfo struct {
	Version string
	Ready   bool
	Found   bool
}

// Level is how a doctor check came out.
type Level int

// Doctor check levels, in the order b2bk doctor ranks them.
const (
	LevelOK Level = iota
	LevelInfo
	LevelWarn
	LevelFail
)

// Step is a next step a doctor check suggests, as a TUI key.
type Step string

// Next steps a doctor check can suggest.
const (
	StepLogs   Step = "logs"
	StepBackup Step = "backup"
	StepResume Step = "resume"
)

// Check is one b2bk doctor check.
type Check struct {
	Subject string
	Level   Level
	Summary string
	Detail  []string
	Next    []Step
}

// Snapshot is a restic snapshot.
type Snapshot struct {
	ID       string
	ShortID  string
	Time     time.Time
	Paths    []string
	Hostname string
	Tags     []string
	Size     uint64
	HasSize  bool
}

// Node is a file or directory in a snapshot.
type Node struct {
	Path  string
	Type  string
	Size  uint64
	Mode  os.FileMode
	UID   uint32
	MTime time.Time
}

// Target is where a restore goes.
type Target int

// Restore targets, safest first.
const (
	TargetNewClaim Target = iota
	TargetDownload
	TargetInPlace
	TargetDatabase
)

// RestoreRequest is one b2bk restore.
type RestoreRequest struct {
	Namespace string
	Backup    string
	Snapshot  string
	Tag       string
	Target    Target
	Claim     string
	Size      string
	Dir       string
	Path      string
	Delete    bool
	DryRun    bool
	// Confirm is what was typed to confirm an overwriting restore.
	Confirm string
}

// DownloadRequest copies Path out of Snapshot into Dir. Folder says Path is a
// folder rather than a file.
type DownloadRequest struct {
	Namespace string
	Backup    string
	Snapshot  string
	Tag       string
	Path      string
	Folder    bool
	Dir       string
}

// ErrNoTerminal means the TUI was asked for without a terminal to draw on.
var ErrNoTerminal = errors.New("the TUI needs an interactive terminal")

// Run draws the TUI on the terminal until the user quits, then removes every
// pod it started, also when it is interrupted or panics.
func Run(ctx context.Context, b Backend, opts Options, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)
	defer signal.Stop(hangup)
	go func() {
		select {
		case <-hangup:
			cancel()
		case <-ctx.Done():
		}
	}()

	m := New(ctx, b, opts)
	defer m.Close()

	popts := append(programOptions(opts, out, os.Environ(), nil),
		tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	p := tea.NewProgram(m, popts...)
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return context.Canceled
	}
	if errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("running the TUI: %w", err)
	}
	return nil
}

// programOptions fixes the color profile the program draws with, and passes
// every message through filter when it is not nil.
func programOptions(opts Options, out io.Writer, environ []string, filter func(tea.Model, tea.Msg) tea.Msg) []tea.ProgramOption {
	profile := programProfile(opts, out, environ)
	keep := func(m tea.Model, msg tea.Msg) tea.Msg {
		msg = keepProfile(profile, msg)
		if filter != nil && msg != nil {
			msg = filter(m, msg)
		}
		return msg
	}
	return []tea.ProgramOption{tea.WithColorProfile(profile), tea.WithFilter(keep)}
}

// programProfile is the color profile the terminal supports, with no color
// at all under NoColor. colorprofile alone honors only a NO_COLOR that parses
// as true, where https://no-color.org means any value but the empty string.
func programProfile(opts Options, out io.Writer, environ []string) colorprofile.Profile {
	profile := colorprofile.Detect(out, environ)
	if opts.NoColor {
		profile = min(profile, colorprofile.ASCII)
	}
	return profile
}

// keepProfile stops a terminal's answer to a capability query from raising
// the color profile above max, which Bubble Tea otherwise does on RGB or Tc.
func keepProfile(maxProfile colorprofile.Profile, msg tea.Msg) tea.Msg {
	switch msg := msg.(type) {
	case tea.CapabilityMsg:
		if (msg.Content == "RGB" || msg.Content == "Tc") && maxProfile < colorprofile.TrueColor {
			return nil
		}
	case tea.ColorProfileMsg:
		if msg.Profile > maxProfile {
			return tea.ColorProfileMsg{Profile: maxProfile}
		}
	}
	return msg
}
