package tui

import (
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"
)

func TestNextRunIsInTheBackupsTimeZone(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	m := newTestModel(t, b, allNamespaces)
	start(t, m)
	chicago, err := time.LoadLocation(testZone)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		ns   string
		want time.Time
		ok   bool
	}{
		{nsKosync, time.Date(2026, 10, 12, 9, 15, 0, 0, chicago), true},
		{"apprise", time.Date(2026, 10, 12, 1, 36, 0, 0, chicago), true},
		{"wx", time.Time{}, false},
	}
	for _, tt := range tests {
		next, ok := m.rowFor(key(tt.ns)).nextRun(testNow)
		if ok != tt.ok || !next.Equal(tt.want) {
			t.Errorf("%s: next = %v %t, want %v %t", tt.ns, next, ok, tt.want, tt.ok)
		}
		if ok && next.Location().String() != testZone {
			t.Errorf("%s: next is in %s, want %s", tt.ns, next.Location(), testZone)
		}
	}
}

func TestHeldBackupsDoNotFire(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeBackend(t), allNamespaces)
	start(t, m)
	r := m.rowFor(key(nsKosync))
	r.hold = "a restore is running (b2bk pid 1)"
	if _, ok := r.nextRun(testNow); ok {
		t.Error("a held backup has a next run")
	}
	if r.health() != healthHeld {
		t.Errorf("health = %v, want held", r.health())
	}
	r.cj.Spec.Suspend = ptr.To(true)
	r.hold = ""
	if _, ok := r.nextRun(testNow); ok {
		t.Error("a suspended CronJob has a next run")
	}
}

func TestParseProgress(t *testing.T) {
	t.Parallel()
	const kib12, mib800 = "1.200 KiB", "800.001 MiB"
	tests := []struct {
		line    string
		ok      bool
		percent float64
		done    string
		total   string
		eta     time.Duration
	}{
		{"[2:07] 61.00%  25511 files 1.182 GiB, total 41822 files 1.937 GiB, 0 errors ETA 1:20", true, 61,
			"1.182 GiB", "1.937 GiB", 80 * time.Second},
		{"[1:02:03] 5.50%  10 files 20.000 MiB, total 400 files 1.000 GiB, 2 errors ETA 1:00:00", true, 5.5,
			"20.000 MiB", "1.000 GiB", time.Hour},
		{"[0:01] 100.00%  3 files/dirs 1.200 KiB, total 3 files/dirs 1.200 KiB", true, 100, kib12, kib12, 0},
		{"[0:05] 1200 files, 2.000 MiB, 0 errors", true, 0, "2.000 MiB", "?", 0},
		{"[0:01] 100.00%  3 files/dirs 1.200 KiB, total 3 files/dirs 1.200 KiB, skipped 1 files/dirs 2 B", true, 100,
			kib12, kib12, 0},
		{"[0:00] 14.96%  200 files 119.651 MiB, total 201 files 800.001 MiB, 0 errors ETA 0:01", true, 14.96,
			"119.651 MiB", mib800, time.Second},
		{"[0:10] 0 files 8.231 MiB, total 1 files 0 B, 0 errors", true, 0, "8.231 MiB", "?", 0},
		{"[0:00] 27.33%  200 files/dirs 218.670 MiB, total 201 files/dirs 800.001 MiB", true, 27.33,
			"218.670 MiB", mib800, 0},
		{"[0:00] 100.00%  1 / 1 snapshots", false, 0, "", "", 0},
		{"scan finished in 32.118s: 41822 files, 1.937 GiB", false, 0, "", "", 0},
		{"open repository", false, 0, "", "", 0},
	}
	for _, tt := range tests {
		p, ok := parseProgress(tt.line)
		if ok != tt.ok || p.percent != tt.percent || p.done != tt.done || p.total != tt.total || p.eta != tt.eta {
			t.Errorf("parseProgress(%q) = %+v %t", tt.line, p, ok)
		}
	}
}

func TestCommandLabel(t *testing.T) {
	t.Parallel()
	const filesData = "files /data"
	tests := map[string]string{
		"+ restic backup --retry-lock=5m0s --tag=files /data":        filesData,
		"+ restic backup --tag=db --stdin --stdin-from-command x":    "db x",
		"+ restic forget --prune --tag=files":                        "",
		"restic backup /data":                                        "",
		"+ /usr/bin/restic backup --tag=files /data --group-by=tags": filesData,
		"+ /usr/bin/restic restore latest:/data --target=/data":      "latest:/data",
	}
	for in, want := range tests {
		if got := commandLabel(in); got != want {
			t.Errorf("commandLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStreamTakesProgressOutOfTheLog(t *testing.T) {
	t.Parallel()
	s := startStream(t.Context(), newHub(), testNow, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, giteaLog)
		return err
	})
	eventually(t, func() bool { return s.snapshot().done })
	v := s.snapshot()
	if len(v.lines) != 9 || v.progress == nil || v.label != "files /data" {
		t.Fatalf("lines = %d, progress = %+v, label = %q", len(v.lines), v.progress, v.label)
	}
	if v.lines[0].at.IsZero() || v.lines[0].text != "+ restic backup --retry-lock=5m0s --tag=files /data" {
		t.Errorf("the timestamp was not split off: %+v", v.lines[0])
	}
}

// resticLog is what restic 0.19.1 in the backup image writes with
// RESTIC_PROGRESS_FPS set and no terminal, as kubectl logs --timestamps shows
// it: a status line with the files being read under it, and the wrapper's
// trace of the real binary.
const resticLog = `2026-10-12T01:06:08.000000000Z + /usr/bin/restic backup --tag=files /w/data --group-by=tags
2026-10-12T01:06:08.100000000Z no parent snapshot found, will read all files
2026-10-12T01:06:08.500000000Z [0:00] 14.96%  200 files 119.651 MiB, total 201 files 800.001 MiB, 0 errors ETA 0:01
2026-10-12T01:06:08.500000000Z /w/data/big
2026-10-12T01:06:09.000000000Z [0:01] 200 files 716.626 MiB, total 201 files 800.001 MiB, 0 errors
2026-10-12T01:06:09.000000000Z /w/data/big
2026-10-12T01:06:09.200000000Z 
2026-10-12T01:06:09.200000000Z Files:         201 new,     0 changed,     0 unmodified
2026-10-12T01:06:09.200000000Z Dirs:            2 new,     0 changed,     0 unmodified
2026-10-12T01:06:09.200000000Z Added to the repository: 800.099 MiB (800.110 MiB stored)
2026-10-12T01:06:09.200000000Z 
2026-10-12T01:06:09.200000000Z processed 201 files, 800.001 MiB in 0:02
2026-10-12T01:06:09.200000000Z snapshot 4b28f2bb saved
`

func TestStreamReadsResticsOwnStatusLines(t *testing.T) {
	t.Parallel()
	s := startStream(t.Context(), newHub(), testNow, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, resticLog)
		return err
	})
	eventually(t, func() bool { return s.snapshot().done })
	v := s.snapshot()
	if v.label != "files /w/data" || v.progress == nil || v.progress.done != "716.626 MiB" ||
		v.progress.total != "800.001 MiB" || v.progress.hasPercent {
		t.Fatalf("label = %q, progress = %+v", v.label, v.progress)
	}
	texts := make([]string, 0, len(v.lines))
	for _, l := range v.lines {
		texts = append(texts, l.text)
	}
	got := strings.Join(texts, "\n")
	if strings.Contains(got, "/w/data/big") || strings.Contains(got, "[0:0") {
		t.Errorf("status lines or the files under them are left in the log:\n%s", got)
	}
	if len(v.lines) != 9 || !strings.HasPrefix(got, "+ /usr/bin/restic backup") ||
		!strings.HasSuffix(got, "snapshot 4b28f2bb saved") {
		t.Errorf("log = %d lines:\n%s", len(v.lines), got)
	}
}

func TestLoadOptionsPrecedence(t *testing.T) {
	t.Parallel()
	yes, no := ptr.To(true), ptr.To(false)
	tests := []struct {
		name  string
		file  string
		env   map[string]string
		flags Flags
		want  Options
		err   bool
	}{
		{name: "defaults", want: Options{Mouse: true}},
		{name: "file", file: "tui:\n  allNamespaces: true\n  mouse: false\n", want: Options{AllNamespaces: true}},
		{name: "env over file", file: "tui:\n  allNamespaces: true\n",
			env: map[string]string{EnvAllNamespaces: "f", EnvMouse: "0"}, want: Options{}},
		{name: "flags over env", env: map[string]string{EnvAllNamespaces: "false", EnvMouse: "false"},
			flags: Flags{AllNamespaces: yes, Mouse: yes}, want: Options{AllNamespaces: true, Mouse: true}},
		{name: "flag false over file", file: "tui:\n  mouse: true\n", flags: Flags{Mouse: no}, want: Options{}},
		{name: "bad env", env: map[string]string{EnvMouse: "sometimes"}, err: true},
		{name: "unknown key", file: "tui:\n  mosue: false\n", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.file != "" {
				path := filepath.Join(dir, "b2bk", "config.yaml")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{"XDG_CONFIG_HOME": dir}
			maps.Copy(env, tt.env)
			got, err := LoadOptions(tt.flags, func(k string) string { return env[k] })
			if (err != nil) != tt.err {
				t.Fatalf("err = %v, want error %t", err, tt.err)
			}
			if !tt.err && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConfigPathFallsBackToHome(t *testing.T) {
	t.Parallel()
	got, err := ConfigPath(func(k string) string {
		if k == "HOME" {
			return "/home/u"
		}
		return ""
	})
	if err != nil || got != "/home/u/.config/b2bk/config.yaml" {
		t.Errorf("ConfigPath = %q, %v", got, err)
	}
}

func TestReviewAccess(t *testing.T) {
	t.Parallel()
	b := newFakeBackend(t)
	b.denied["delete jobs"] = true
	b.denied["watch pods"] = true
	cs, err := b.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	a := reviewAccess(t.Context(), cs, "")
	if a.can(actCancel) || !a.can(actBackup) || !a.can(actPod) {
		t.Errorf("denied = %v", a.denied)
	}
	if a.denied[actCancel] != "RBAC does not let you delete jobs in every namespace" {
		t.Errorf("reason = %q", a.denied[actCancel])
	}
	if a.unwatched[KindPod] == "" || len(a.watched()) != 4 {
		t.Errorf("unwatched = %v", a.unwatched)
	}
}

func TestFormatting(t *testing.T) {
	t.Parallel()
	for in, want := range map[uint64]string{451: "451 B", 1126: "1.1 KiB", 50541773: "48.2 MiB", 1 << 40: "1.0 TiB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{24 * time.Second: "24s", 62 * time.Second: "1m02s",
		134 * time.Second: "2m14s", 2*time.Hour + 5*time.Minute: "2h05m"} {
		if got := clock(in); got != want {
			t.Errorf("clock(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{57 * time.Minute: "57m", 181 * time.Minute: "3h01m",
		50 * time.Hour: "2d02h"} {
		if got := span(in); got != want {
			t.Errorf("span(%v) = %q, want %q", in, got, want)
		}
	}
	if got := commas(2119); got != "2,119" {
		t.Errorf("commas = %q", got)
	}
	for in, want := range map[string]string{"/app.dmp": "custom format", "/app.sql": "SQL dump",
		"/app": "plain SQL (old)"} {
		if got := dumpFormat(in); got != want {
			t.Errorf("dumpFormat(%q) = %q, want %q", in, got, want)
		}
	}
	if got := fit("vaultwarden", 6); got != "vault…" {
		t.Errorf("fit = %q", got)
	}
}

func TestBuildTreeCountsFilesBelowEachFolder(t *testing.T) {
	t.Parallel()
	tr := buildTree(exampleNodes())
	if got := tr.files["/data"]; got != 9 {
		t.Errorf("files under /data = %d, want 9", got)
	}
	if got := tr.files["/data/attachments"]; got != 3 {
		t.Errorf("files under attachments = %d, want 3", got)
	}
	kids := tr.children["/data"]
	if kids[0].Type != nodeDir || kids[len(kids)-1].Path != "/data/rsa_key.pub.pem" {
		t.Errorf("folders do not come first: %+v", kids)
	}
}
