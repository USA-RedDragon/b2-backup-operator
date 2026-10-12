package tui

import (
	"strings"
	"testing"
)

// wizardAt opens the restore wizard on vaultwarden's latest files snapshot and
// moves to target.
func wizardAt(t *testing.T, b *fakeBackend, target Target) (*Model, *restoreScreen) {
	t.Helper()
	m := dashboardOn(t, b, nsVault)
	press(t, m, "s")
	eventually(t, func() bool { return m.sessions.get(key(nsVault)).view().snapshotsDone })
	press(t, m, "r")
	w := m.top().(*restoreScreen)
	for w.target != target {
		before := w.target
		press(t, m, "down")
		if w.target == before {
			t.Fatalf("cannot reach %v from %v", target, w.target)
		}
	}
	return m, w
}

func realRestores(b *fakeBackend) []RestoreRequest {
	var out []RestoreRequest
	for _, r := range b.restores() {
		if !r.DryRun {
			out = append(out, r)
		}
	}
	return out
}

func TestOverwritingRestoreNeedsTheNameTyped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		typed string
		runs  bool
	}{
		{"nothing typed", "", false},
		{"part of the name", "rest", false},
		{"another name", "vaultwarden", false},
		{"the name with a space", "restic ", false},
		{"the name", nameRestic, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := newFakeBackend(t)
			m, w := wizardAt(t, b, TargetInPlace)
			press(t, m, "enter")
			eventually(t, func() bool { return w.dry != nil && w.dry.snapshot().done })
			typeText(t, m, tt.typed)
			press(t, m, "enter")
			got := realRestores(b)
			if !tt.runs {
				if len(got) != 0 {
					t.Fatalf("restored after typing %q: %+v", tt.typed, got)
				}
				if !strings.Contains(w.confirmErr, "type restic exactly") {
					t.Errorf("confirmErr = %q", w.confirmErr)
				}
				return
			}
			eventually(t, func() bool { return len(realRestores(b)) == 1 })
			req := realRestores(b)[0]
			if req.Target != TargetInPlace || req.Confirm != nameRestic || req.Snapshot != b.snapshots[0].ID {
				t.Errorf("request = %+v", req)
			}
		})
	}
}

func TestOverwritingRestoreWaitsForItsDryRun(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m, w := wizardAt(t, b, TargetInPlace)
	press(t, m, "enter")
	eventually(t, func() bool { return w.dry != nil && w.dry.snapshot().done })
	dry := b.restores()
	if len(dry) != 1 || !dry[0].DryRun || dry[0].Confirm != "" {
		t.Fatalf("the dry run was %+v, want one unconfirmed dry run", dry)
	}
	w.dry = &stream{err: errGone, done: true, cancel: func() {}}
	typeText(t, m, nameRestic)
	press(t, m, "enter")
	if len(realRestores(b)) != 0 {
		t.Error("a restore ran after its dry run failed")
	}
}

func TestANewClaimNeedsNoTypedName(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m, w := wizardAt(t, b, TargetNewClaim)
	if w.typing(m) {
		t.Fatal("the target list takes text")
	}
	press(t, m, "space")
	press(t, m, "tab")
	typeText(t, m, "-x")
	press(t, m, "enter")
	eventually(t, func() bool { return w.dry != nil && w.dry.snapshot().done })
	press(t, m, "enter")
	eventually(t, func() bool { return len(realRestores(b)) == 1 })
	req := realRestores(b)[0]
	if req.Target != TargetNewClaim || req.Confirm != "" || req.Claim != "vaultwarden-restore-1011-x" {
		t.Errorf("request = %+v", req)
	}
}

func TestUnavailableTargetsAreSkipped(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.targets = map[Target]string{
		TargetInPlace:  "the operator has not rendered this backup's current spec",
		TargetDatabase: "this backup has no database",
	}
	m, w := wizardAt(t, b, TargetDownload)
	press(t, m, "down", "down")
	if w.target != TargetDownload {
		t.Errorf("down moved onto an unavailable target: %v", w.target)
	}
	if !strings.Contains(screenText(m), "- Over the live volume        unavailable: the operator has not rendered") {
		t.Errorf("the reason is not shown:\n%s", screenText(m))
	}
}

func TestOptionsReachTheRequest(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m, w := wizardAt(t, b, TargetInPlace)
	for w.focus != focusDelete {
		press(t, m, "tab")
	}
	press(t, m, "space", "tab", "space", "shift+tab", "shift+tab")
	if w.focus != focusPath {
		t.Fatalf("focus = %d, want the path field", w.focus)
	}
	typeText(t, m, "/db.sqlite3")
	press(t, m, "enter")
	typeText(t, m, nameRestic)
	press(t, m, "enter")
	eventually(t, func() bool { return len(realRestores(b)) == 1 })
	req := realRestores(b)[0]
	if !req.Delete || req.Path != "/data/db.sqlite3" || !w.onlyPath {
		t.Errorf("request = %+v", req)
	}
	if len(b.restores()) != 1 {
		t.Errorf("a dry run ran with dry run turned off: %+v", b.restores())
	}
}

func TestSummarizeDryRun(t *testing.T) {
	t.Parallel()
	var l []logLine
	for line := range strings.SplitSeq(newFakeBackend(t).dryRun+"restored  /data\ndeleted   /data/old\n", "\n") {
		l = append(l, logLine{text: line})
	}
	got := summarizeDryRun(l, TargetInPlace)
	if got.text != "3 files would change, 1 removed, 2 unchanged" {
		t.Errorf("text = %q", got.text)
	}
	if len(got.items) != 4 || !strings.HasSuffix(got.items[1], "modified") {
		t.Errorf("items = %q", got.items)
	}
	ls := []logLine{{text: "-rw-r--r--  1000  1000   1126 2026-10-11 03:05:00 /data/config.json"},
		{text: "drwxr-xr-x     0     0      0 2026-10-11 03:05:00 /data"}}
	if got := summarizeDryRun(ls, TargetDownload); got.text != "2 files and folders would be restored" {
		t.Errorf("download text = %q", got.text)
	}
}

func TestOverwritingTargetsNeedToHoldTheSchedule(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.denied["patch scheduledbackups"] = true
	b.targets = map[Target]string{}
	m, w := wizardAt(t, b, TargetDownload)
	press(t, m, "down", "down")
	if w.target != TargetDownload {
		t.Errorf("down reached %v without RBAC to hold the schedule", w.target)
	}
	for _, target := range []Target{TargetInPlace, TargetDatabase} {
		if !strings.Contains(w.reasons[target], "patch scheduledbackups") {
			t.Errorf("reason for %v = %q", target, w.reasons[target])
		}
	}
}
