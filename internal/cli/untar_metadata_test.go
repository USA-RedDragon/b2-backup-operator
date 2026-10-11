package cli_test

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
)

type tarEntry struct {
	name  string
	kind  byte
	mode  int64
	mtime time.Time
	body  string
	uid   int
}

func TestUntarKeepsModesTimesAndOwners(t *testing.T) {
	t.Parallel()
	old := time.Date(2024, 3, 14, 15, 9, 26, 0, time.UTC)
	older := time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC)
	foreignUID := 54321
	entries := []tarEntry{
		{name: "data/", kind: tar.TypeDir, mode: 0o750, mtime: older},
		{name: "data/app.db", kind: tar.TypeReg, mode: 0o640, mtime: old, body: "db", uid: foreignUID},
		{name: "data/run.sh", kind: tar.TypeReg, mode: 0o755, mtime: old, body: "#!/bin/sh"},
		{name: "data/locked/", kind: tar.TypeDir, mode: 0o555, mtime: older},
		{name: "data/locked/key", kind: tar.TypeReg, mode: 0o400, mtime: old, body: "k"},
		{name: "data/shared/", kind: tar.TypeDir, mode: 0o2775, mtime: older},
	}
	dest := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "data", "locked"), 0o755) })

	if _, err := cli.Untar(bytes.NewReader(tarOf(t, entries)), dest); err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			info, err := os.Lstat(filepath.Join(dest, filepath.FromSlash(e.name)))
			if err != nil {
				t.Fatal(err)
			}
			want := fs.FileMode(e.mode) & fs.ModePerm
			if e.mode&0o2000 != 0 {
				want |= fs.ModeSetgid
			}
			if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid); got != want {
				t.Errorf("mode = %v, want %v", got, want)
			}
			if !info.ModTime().Equal(e.mtime) {
				t.Errorf("mtime = %v, want %v", info.ModTime(), e.mtime)
			}
			if os.Geteuid() == 0 {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != e.uid {
					t.Errorf("uid = %d, want %d", st.Uid, e.uid)
				}
			}
		})
	}
}

func tarOf(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{
			Name: e.name, Typeflag: e.kind, Mode: e.mode, ModTime: e.mtime, AccessTime: e.mtime,
			Size: int64(len(e.body)), Uid: e.uid, Format: tar.FormatPAX,
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
