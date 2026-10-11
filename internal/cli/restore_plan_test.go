package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const (
	snapFiles   = "aaaaaaaa11111111"
	snapOlder   = "bbbbbbbb22222222"
	snapDB      = "cccccccc33333333"
	snapOldDump = "dddddddd44444444"
	snapStash   = "eeeeeeee55555555"
	snapOther   = "ffffffff66666666"
	snapSub     = "1234567812345678"
)

var day = time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)

func snapshotsFixture() []resticSnapshot {
	return []resticSnapshot{
		{ID: snapOlder, Time: day.Add(-24 * time.Hour), Paths: []string{dataDir}, Hostname: testNS, Tags: []string{filesTag}},
		{ID: snapFiles, Time: day, Paths: []string{dataDir}, Hostname: testNS, Tags: []string{filesTag}},
		{ID: snapDB, Time: day.Add(time.Hour), Paths: []string{"/" + testNS + ".dmp"}, Hostname: testNS, Tags: []string{"db"}},
		{ID: snapOldDump, Time: day.Add(-48 * time.Hour), Paths: []string{"/" + testNS}, Hostname: testNS, Tags: []string{"db"}},
		{ID: snapStash, Time: day.Add(2 * time.Hour), Paths: []string{dataDir, blobsDir}, Hostname: testNS, Tags: []string{filesTag}},
		{ID: snapOther, Time: day.Add(3 * time.Hour), Paths: []string{dataDir}, Hostname: "renamed", Tags: []string{filesTag}},
		{ID: snapSub, Time: day.Add(4 * time.Hour), Paths: []string{"/data/app"}, Hostname: testNS, Tags: []string{filesTag}},
	}
}

func byID(id string) []resticSnapshot {
	return slices.DeleteFunc(snapshotsFixture(), func(s resticSnapshot) bool { return s.ID != id })
}

func TestParseSnapshotsReadsFlatAndGroupedOutput(t *testing.T) {
	t.Parallel()
	flat := `[{"id":"a1","short_id":"a1","time":"2026-10-11T06:24:00Z","paths":["/data"],"hostname":"ns","tags":["files"]}]`
	grouped := `[{"group_key":{"hostname":"","paths":null,"tags":["files"]},"snapshots":[` +
		`{"id":"a1","time":"2026-10-11T06:24:00Z","paths":["/data"],"hostname":"ns","tags":["files"]},` +
		`{"id":"a2","time":"2026-10-11T07:24:00Z","paths":["/data"],"hostname":"ns","tags":["files"]}]}]`
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"flat", flat, []string{"a1"}},
		{"grouped", grouped, []string{"a1", "a2"}},
		{"empty", "  \n", nil},
		{"none", "[]", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snaps, err := parseSnapshots([]byte(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, s := range snaps {
				ids = append(ids, s.ID)
			}
			if !slices.Equal(ids, tt.want) {
				t.Errorf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
	if _, err := parseSnapshots([]byte("not json")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestChooseSnapshot(t *testing.T) {
	t.Parallel()
	files := restoreSource{tag: filesTag, host: testNS, paths: []string{dataDir}}
	stash := restoreSource{tag: filesTag, host: testNS, paths: []string{blobsDir, dataDir}}
	db := restoreSource{tag: "db", host: testNS}
	tests := []struct {
		name     string
		snapshot string
		snaps    []resticSnapshot
		src      restoreSource
		want     string
		wantErr  error
	}{
		{"the latest of exactly the source's paths, on its host", latest, snapshotsFixture(), files, snapFiles, nil},
		{"the latest of a multi-path source", latest, snapshotsFixture(), stash, snapStash, nil},
		{"the latest dump, never a files snapshot", latest, snapshotsFixture(), db, snapDB, nil},
		{"nothing matches", latest, byID(snapOther), files, "", nil},
		{"a named snapshot of the source", snapOlder, byID(snapOlder), files, snapOlder, nil},
		{"a named snapshot under the source path", snapSub, byID(snapSub), files, snapSub, nil},
		{"a named snapshot from before a host rename", snapOther, byID(snapOther), files, snapOther, nil},
		{"a named dump restored over files", snapDB, byID(snapDB), files, "", ErrWrongSnapshot},
		{"a named files snapshot restored into a database", snapFiles, byID(snapFiles), db, "", ErrWrongSnapshot},
		{"a named old plain-SQL dump", snapOldDump, byID(snapOldDump), db, snapOldDump, nil},
		{"a named snapshot of another path", snapStash, byID(snapStash), files, "", ErrWrongSnapshot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := &restoreOptions{snapshot: tt.snapshot}
			got, err := o.chooseSnapshot(tt.snaps, tt.src)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.want == "":
				if err == nil {
					t.Errorf("chose %s, want an error", got.ID)
				}
			case err != nil || got.ID != tt.want:
				t.Errorf("chose %s, %v; want %s", got.ID, err, tt.want)
			}
		})
	}
}

func TestSnapshotsQueryFiltersTheLatestAndNamesAnID(t *testing.T) {
	t.Parallel()
	src := restoreSource{tag: filesTag, host: testNS, paths: []string{blobsDir, dataDir}}
	got := strings.Join((&restoreOptions{snapshot: latest}).snapshotsQuery(src), " ")
	if want := "snapshots --json --group-by= --tag=files --host=" + testNS + " --path=/blobs --path=/data"; got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
	got = strings.Join((&restoreOptions{snapshot: snapOlder}).snapshotsQuery(src), " ")
	if want := "snapshots --json --group-by= " + snapOlder; got != want {
		t.Errorf("query = %q, want %q", got, want)
	}
}

func TestRestoreSteps(t *testing.T) {
	t.Parallel()
	single := resticSnapshot{ID: snapFiles, Paths: []string{dataDir}}
	multi := resticSnapshot{ID: snapStash, Paths: []string{dataDir, blobsDir}}
	tests := []struct {
		name    string
		snap    resticSnapshot
		path    string
		want    []restoreStep
		wantErr string
	}{
		{"everything", multi, "", []restoreStep{{snapPath: blobsDir}, {snapPath: dataDir}}, ""},
		{"a relative path", single, "app/config.php", []restoreStep{{snapPath: dataDir, sub: "/app/config.php"}}, ""},
		{"an absolute path", multi, "/blobs/x", []restoreStep{{snapPath: blobsDir, sub: "/x"}}, ""},
		{"the snapshot path itself", single, dataDir, []restoreStep{{snapPath: dataDir}}, ""},
		{"a relative path with several roots", multi, "x", nil, "give an absolute path"},
		{"a path outside the snapshot", single, "/etc/passwd", nil, "is not in snapshot"},
		{"a sibling with a shared stem", single, "/database", nil, "is not in snapshot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := (&restoreOptions{path: tt.path}).steps(tt.snap)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tt.want) {
				t.Errorf("steps = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

type fakePod struct {
	ls       string
	dump     []byte
	commands []string
}

func (p *fakePod) exec(_ context.Context, argv []string, stdout io.Writer) error {
	line := strings.Join(argv, " ")
	p.commands = append(p.commands, line)
	switch {
	case len(argv) > 1 && argv[1] == "snapshots":
		var out []resticSnapshot
		if strings.Contains(line, "--host=") {
			out = snapshotsFixture()
		} else {
			out = byID(argv[len(argv)-1])
		}
		raw := "["
		for i, s := range out {
			if i > 0 {
				raw += ","
			}
			raw += fmt.Sprintf(`{"id":%q,"time":%q,"paths":["%s"],"hostname":%q,"tags":["%s"]}`,
				s.ID, s.Time.Format(time.RFC3339), strings.Join(s.Paths, `","`), s.Hostname, strings.Join(s.Tags, `","`))
		}
		_, err := io.WriteString(stdout, raw+"]")
		return err
	case len(argv) > 1 && argv[1] == "ls":
		_, err := io.WriteString(stdout, p.ls)
		return err
	case len(argv) > 1 && argv[1] == "dump":
		_, err := stdout.Write(p.dump)
		return err
	}
	return nil
}

func stashBackup() *b2backupv1.ScheduledBackup {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: "config", MountPath: dataDir}, {ExistingClaim: "blobs"}}
	sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles, Paths: []string{dataDir, blobsDir}}}
	return sb
}

func filesBackupAt() *b2backupv1.ScheduledBackup {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: testClaimName, MountPath: dataDir}}
	sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}
	return sb
}

func TestVolumeRestoresPutFilesBackWhereTheyWere(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sb      *b2backupv1.ScheduledBackup
		o       restoreOptions
		target  func(*b2backupv1.ScheduledBackup) volumeTarget
		want    []string
		wantErr string
	}{
		{
			name:   "in place",
			sb:     filesBackupAt(),
			o:      restoreOptions{snapshot: latest, delete: true},
			target: inPlaceTarget,
			want:   []string{"restic restore " + snapFiles + ":/data --target=/data --delete"},
		},
		{
			name:   "in place, both claims of a multi-path source",
			sb:     stashBackup(),
			o:      restoreOptions{snapshot: latest},
			target: inPlaceTarget,
			want: []string{
				"restic restore " + snapStash + ":/blobs --target=/blobs",
				"restic restore " + snapStash + ":/data --target=/data",
			},
		},
		{
			name:   "in place, one file",
			sb:     filesBackupAt(),
			o:      restoreOptions{snapshot: latest, path: "app/config.php", exclude: []string{"*.log"}},
			target: inPlaceTarget,
			want: []string{"restic restore " + snapFiles + ":/data --target=/data --include=/app/config.php " +
				"--exclude=*.log"},
		},
		{
			name: "a new claim laid out like the source",
			sb:   filesBackupAt(),
			o:    restoreOptions{snapshot: snapSub},
			target: func(sb *b2backupv1.ScheduledBackup) volumeTarget {
				return newClaimTarget(sb, &sb.Spec.Volumes[0], restoreMountPath)
			},
			want: []string{"restic restore " + snapSub + ":/data/app --target=/restore/app"},
		},
		{
			name:    "never a dump over files",
			sb:      filesBackupAt(),
			o:       restoreOptions{snapshot: snapDB},
			target:  inPlaceTarget,
			wantErr: "not files",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src, err := tt.o.filesSource(tt.sb)
			if err != nil {
				t.Fatal(err)
			}
			pod := &fakePod{}
			err = tt.o.restoreVolumes(context.Background(), pod, src, tt.target(tt.sb), io.Discard)
			if tt.wantErr != "" {
				if !errors.Is(err, ErrWrongSnapshot) {
					t.Errorf("err = %v, want ErrWrongSnapshot", err)
				}
				if len(pod.commands) != 1 {
					t.Errorf("ran %v after refusing the snapshot", pod.commands[1:])
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := pod.commands[1:]; !slices.Equal(got, tt.want) {
				t.Errorf("commands =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

func TestANewClaimCopiesOneClaim(t *testing.T) {
	t.Parallel()
	sb := stashBackup()
	src, err := (&restoreOptions{}).filesSource(sb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceVolume(sb, src); err == nil || !strings.Contains(err.Error(), "spans") {
		t.Errorf("err = %v, want a source on two claims refused", err)
	}
}

func TestFilesSourceSelection(t *testing.T) {
	t.Parallel()
	two := filesBackupAt()
	two.Spec.Sources = []b2backupv1.BackupSource{
		{Type: b2backupv1.SourceTypeFiles, Tag: serverTag, Paths: []string{dataDir}},
		{Type: b2backupv1.SourceTypeFiles, Tag: filebrowserTag, Paths: []string{"/data/fb"}},
		{Type: b2backupv1.SourceTypeCNPG},
	}
	if _, err := (&restoreOptions{}).filesSource(two); !errors.Is(err, ErrNoFilesSource) ||
		!strings.Contains(err.Error(), "--tag") {
		t.Errorf("err = %v, want --tag asked for", err)
	}
	src, err := (&restoreOptions{tag: filebrowserTag}).filesSource(two)
	if err != nil || src.tag != filebrowserTag || !slices.Equal(src.paths, []string{"/data/fb"}) {
		t.Errorf("source = %+v, %v", src, err)
	}
	if _, err := (&restoreOptions{tag: "db"}).filesSource(two); !errors.Is(err, ErrNoFilesSource) {
		t.Errorf("err = %v, want a database tag refused as a files source", err)
	}
	script := newBackup(testBackupName, testRepoName)
	script.Spec.Script = "restic backup /backup/wx.db"
	if _, err := (&restoreOptions{}).filesSource(script); !errors.Is(err, ErrNoFilesSource) {
		t.Errorf("err = %v, want a script backup refused", err)
	}
}

func TestDatabaseRestoreReadsTheDumpTheSnapshotHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		snapshot string
		want     string
	}{
		{"the latest custom-format dump", latest, "restic dump '" + snapDB + "' '/" + testNS + ".dmp' | restoredb cnpg"},
		{"an old plain-SQL dump stored without an extension", snapOldDump,
			"restic dump '" + snapOldDump + "' '/" + testNS + "' | restoredb cnpg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sb := newBackup(testBackupName, testRepoName)
			withDatabase(sb)
			sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}}
			o := &restoreOptions{snapshot: tt.snapshot, dryRun: true}
			src, err := o.databaseSource(sb)
			if err != nil {
				t.Fatal(err)
			}
			pod := &fakePod{}
			if err := o.restoreDatabase(context.Background(), pod, sb, src, "the database", io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			script := pod.commands[len(pod.commands)-1]
			for _, want := range []string{"set -o pipefail", "command -v restoredb", tt.want, "--dry-run"} {
				if !strings.Contains(script, want) {
					t.Errorf("expected %q in:\n%s", want, script)
				}
			}
		})
	}

	script := newBackup(testBackupName, testRepoName)
	withDatabase(script)
	script.Spec.Script = "restic backup --tag=db --tag=app --stdin-filename=app.sql --stdin-from-command -- backup-cnpg.sh"
	if _, err := (&restoreOptions{}).databaseSource(script); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("err = %v, want --tag asked for on a script backup", err)
	}
	if src, err := (&restoreOptions{tag: appDir}).databaseSource(script); err != nil || src.tag != appDir {
		t.Errorf("source = %+v, %v", src, err)
	}
}

func TestDownloadLaysFilesOutUnderTheirAbsolutePaths(t *testing.T) {
	t.Parallel()
	tarball := func(t *testing.T, files map[string]string) []byte {
		t.Helper()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range files {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(tw, body); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	tests := []struct {
		name  string
		o     restoreOptions
		ls    string
		dump  []byte
		file  string
		want  string
		calls string
	}{
		{"a whole source", restoreOptions{snapshot: latest}, "", nil, "data/app/a.txt", "a",
			"restic dump --archive=tar " + snapFiles + ":/data /"},
		{"one file", restoreOptions{snapshot: latest, path: "app/a.txt"},
			`{"type":"file","path":"/app/a.txt"}`, []byte("only a"), "data/app/a.txt", "only a",
			"restic dump " + snapFiles + ":/data /app/a.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dump := tt.dump
			if dump == nil {
				dump = tarball(t, map[string]string{"app/a.txt": "a"})
			}
			pod := &fakePod{ls: tt.ls, dump: dump}
			dest := t.TempDir()
			src, err := tt.o.filesSource(filesBackupAt())
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.o.download(context.Background(), pod, src, dest, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dest, tt.file))
			if err != nil || string(got) != tt.want {
				t.Errorf("%s = %q, %v; want %q", tt.file, got, err, tt.want)
			}
			if last := pod.commands[len(pod.commands)-1]; last != tt.calls {
				t.Errorf("last command = %q, want %q", last, tt.calls)
			}
		})
	}
}
