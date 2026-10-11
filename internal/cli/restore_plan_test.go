package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
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
			got, err := (&restoreOptions{path: tt.path}).steps(tt.snap, nil)
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
	files    []string
	dump     []byte
	commands []string
}

func (p *fakePod) nodes(paths []string) string {
	var out strings.Builder
	for _, path := range paths {
		kind := "dir"
		if slices.Contains(p.files, path) || strings.HasSuffix(path, ".dmp") || path == "/"+testNS {
			kind = "file"
		}
		fmt.Fprintf(&out, "{\"type\":%q,\"path\":%q}\n", kind, path)
	}
	return out.String()
}

func (p *fakePod) exec(_ context.Context, argv []string, stdout io.Writer) error {
	line := strings.Join(argv, " ")
	p.commands = append(p.commands, line)
	switch {
	case len(argv) > 1 && argv[1] == GroupSnapshots:
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
	case len(argv) > 3 && argv[1] == "ls" && argv[2] == "--json" && !strings.Contains(argv[3], ":"):
		_, err := io.WriteString(stdout, p.nodes(argv[4:]))
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
			o:      restoreOptions{snapshot: latest, path: "app/config.php"},
			target: inPlaceTarget,
			want:   []string{"restic restore " + snapFiles + ":/data --target=/data --include=/app/config.php"},
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
			if got := pod.commands[2:]; !slices.Equal(got, tt.want) {
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

func TestDatabaseRestoreGoesIntoTheSourcesDatabase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		database string
		flag     string
		want     string
		target   string
	}{
		{"the source's database", "reporting", "", "--database='reporting'", `"reporting"`},
		{"the flag wins", "reporting", "scratch", "--database='scratch'", `"scratch"`},
		{"the Secret's dbname", "", "", "", "secret/postgresql-app names in dbname"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sb := newBackup(testBackupName, testRepoName)
			sb.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineCNPG}
			sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG, Database: tt.database}}
			o := &restoreOptions{snapshot: latest, database: tt.flag}
			src, err := o.databaseSource(sb)
			if err != nil {
				t.Fatal(err)
			}
			if got := databaseTarget(sb.Spec.Database, src.database); !strings.Contains(got, tt.target) {
				t.Errorf("confirmation names %q, want %q in it", got, tt.target)
			}
			pod := &fakePod{}
			if err := o.restoreDatabase(context.Background(), pod, sb, src, "x", io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			script := pod.commands[len(pod.commands)-1]
			if tt.want != "" && !strings.Contains(script, tt.want) {
				t.Errorf("expected %q in:\n%s", tt.want, script)
			}
			if tt.want == "" && strings.Contains(script, "--database") {
				t.Errorf("a database was named although nothing chose one:\n%s", script)
			}
		})
	}
}

func TestDatabaseRestoreRefusesWhatIsNotOneDump(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		snaps []resticSnapshot
		files []string
		want  string
	}{
		{"dumps of two databases under one tag", []resticSnapshot{
			{ID: "a1", Time: day, Paths: []string{"/astro-processing.sql"}, Hostname: testNS, Tags: []string{"db", "astro-processing"}},
			{ID: "s1", Time: day.Add(time.Minute), Paths: []string{"/schedulerdb.sql"}, Hostname: testNS, Tags: []string{"db", "schedulerdb"}},
		}, []string{"/astro-processing.sql", "/schedulerdb.sql"}, "holds dumps of"},
		{"a files snapshot under the dump's tag", []resticSnapshot{
			{ID: "d1", Time: day, Paths: []string{"/" + testNS + ".dmp"}, Hostname: testNS, Tags: []string{"db"}},
			{ID: "f1", Time: day.Add(time.Minute), Paths: []string{dataDir}, Hostname: testNS, Tags: []string{"db"}},
		}, nil, "holds dumps of"},
		{"a files snapshot alone", []resticSnapshot{
			{ID: "f1", Time: day, Paths: []string{dataDir}, Hostname: testNS, Tags: []string{"db"}},
		}, nil, "not a database dump"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sb := newBackup(testBackupName, testRepoName)
			withDatabase(sb)
			sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}}
			o := &restoreOptions{snapshot: latest}
			src, err := o.databaseSource(sb)
			if err != nil {
				t.Fatal(err)
			}
			pod := &listedPod{files: tt.files, snaps: tt.snaps}
			err = o.restoreDatabase(context.Background(), pod, sb, src, "x", io.Discard, io.Discard)
			if !errors.Is(err, ErrWrongSnapshot) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want ErrWrongSnapshot saying %q", err, tt.want)
			}
			if slices.ContainsFunc(pod.commands, func(c string) bool { return strings.Contains(c, "restoredb") }) {
				t.Error("restoredb ran")
			}
		})
	}

	migrated := []resticSnapshot{
		{ID: "old", Time: day, Paths: []string{"/" + testNS}, Hostname: testNS, Tags: []string{"db"}},
		{ID: verbNew, Time: day.Add(time.Hour), Paths: []string{"/" + testNS + ".dmp"}, Hostname: testNS, Tags: []string{"db"}},
	}
	got, err := (&restoreOptions{snapshot: latest}).chooseSnapshot(migrated, restoreSource{tag: "db", host: testNS})
	if err != nil || got.ID != verbNew {
		t.Errorf("chose %s, %v; want the newest dump, the old plain one being the same series", got.ID, err)
	}
}

type listedPod struct {
	fakePod
	snaps []resticSnapshot
}

func (p *listedPod) exec(ctx context.Context, argv []string, stdout io.Writer) error {
	if len(argv) > 1 && argv[1] == GroupSnapshots {
		p.commands = append(p.commands, strings.Join(argv, " "))
		raw, err := json.Marshal(p.snaps)
		if err != nil {
			return err
		}
		_, err = stdout.Write(raw)
		return err
	}
	return p.fakePod.exec(ctx, argv, stdout)
}

func TestVolumeRestoreArguments(t *testing.T) {
	t.Parallel()
	snap := resticSnapshot{ID: snapFiles, Paths: []string{dataDir}}
	src := restoreSource{tag: filesTag, paths: []string{dataDir},
		exclude: []string{cacheDir, "**/tmp*", "/data/logs", "/elsewhere", "/*/thumbs", "/d?ta/x/*.tmp"}}
	tests := []struct {
		name string
		o    restoreOptions
		step restoreStep
		want []string
		not  []string
	}{
		{"the backup's own excludes protect what it never saved", restoreOptions{delete: true},
			restoreStep{snapPath: dataDir},
			[]string{"--exclude=cache", "--exclude=**/tmp*", "--exclude=/logs", "--exclude=/thumbs",
				"--exclude=/x/*.tmp", "--delete"},
			[]string{"/elsewhere", "--exclude=/data", "--exclude=/\n"}},
		{"a path is matched literally, and the excludes left out", restoreOptions{path: "Movie [1080p]*.mkv"},
			restoreStep{snapPath: dataDir, sub: "/Movie [1080p]*.mkv"},
			[]string{`--include=/Movie \[1080p]\*.mkv`}, []string{"--exclude"}},
		{"a dry run lists every deletion", restoreOptions{delete: true, dryRun: true},
			restoreStep{snapPath: dataDir}, []string{"--dry-run", "--verbose=2"}, []string{"--verbose "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args, err := tt.o.restoreArgs(snap, tt.step, src, dataDir)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(args, " ") + " "
			for _, want := range tt.want {
				if !slices.Contains(args, want) {
					t.Errorf("missing %q in %q", want, got)
				}
			}
			for _, not := range tt.not {
				if strings.Contains(got, not) {
					t.Errorf("unexpected %q in %q", not, got)
				}
			}
		})
	}
}

func TestRestoreArgumentsRestic(t *testing.T) {
	t.Parallel()
	snap := resticSnapshot{ID: snapFiles, Paths: []string{dataDir}}
	excluding := restoreSource{paths: []string{dataDir}, exclude: []string{cacheDir}}
	tests := []struct {
		name string
		o    restoreOptions
		sub  string
		src  restoreSource
		want string
	}{
		{"an include and an exclude", restoreOptions{include: []string{"app/**"}, exclude: []string{"*.log"}},
			"", restoreSource{}, "--exclude cannot be combined"},
		{"a path deleting under a source that excludes", restoreOptions{delete: true},
			appMount, excluding, "would remove what the backup excludes"},
		{"an include beside a path, which restic would OR", restoreOptions{include: []string{"*.db"}},
			appMount, restoreSource{}, "--include cannot be combined with --path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			step := restoreStep{snapPath: dataDir, sub: tt.sub}
			if _, err := tt.o.restoreArgs(snap, step, tt.src, dataDir); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q: restic would refuse, or delete what was never saved", err, tt.want)
			}
		})
	}
}

func TestExcludesThatCannotBeRebasedStopDelete(t *testing.T) {
	t.Parallel()
	snap := resticSnapshot{ID: snapFiles, Paths: []string{dataDir}}
	for _, exclude := range []string{"/**/cache", "/*", "/[d/x"} {
		src := restoreSource{paths: []string{dataDir}, exclude: []string{exclude}}
		if _, err := (&restoreOptions{delete: true}).restoreArgs(snap, restoreStep{snapPath: dataDir}, src, dataDir); err == nil ||
			!strings.Contains(err.Error(), "drop --delete") {
			t.Errorf("exclude %s: err = %v, want --delete refused", exclude, err)
		}
		if _, err := (&restoreOptions{}).restoreArgs(snap, restoreStep{snapPath: dataDir}, src, dataDir); err != nil {
			t.Errorf("exclude %s without --delete: %v", exclude, err)
		}
	}
}

func TestASingleFileSourceRestoresFromItsDirectory(t *testing.T) {
	t.Parallel()
	sb := filesBackupAt()
	sb.Spec.Sources[0].Paths = []string{appDB}
	o := restoreOptions{snapshot: snapOlder}
	src, err := o.filesSource(sb)
	if err != nil {
		t.Fatal(err)
	}
	single := func() *listedPod {
		return &listedPod{files: []string{appDB}, dump: []byte("sqlite"),
			snaps: []resticSnapshot{{ID: snapOlder, Time: day, Paths: []string{appDB}, Hostname: testNS,
				Tags: []string{filesTag}}}}
	}

	pod := single()
	if err := o.restoreVolumes(context.Background(), pod, src, inPlaceTarget(sb), io.Discard); err != nil {
		t.Fatal(err)
	}
	if want := "restic restore " + snapOlder + ":/data --target=/data --include=/app.db"; pod.commands[2] != want {
		t.Errorf("in place ran %q, want %q", pod.commands[2], want)
	}

	pod = single()
	plan, err := o.planVolumes(context.Background(), pod, src, newClaimTarget(sb, &sb.Spec.Volumes[0], restoreMountPath))
	if err != nil || plan.targets[0] != restoreMountPath {
		t.Errorf("new claim target = %v, %v; want %s", plan.targets, err, restoreMountPath)
	}

	pod = single()
	dest := t.TempDir()
	if err := o.download(context.Background(), pod, src, dest, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "data", "app.db")); err != nil || string(got) != "sqlite" {
		t.Errorf("downloaded %q, %v", got, err)
	}
}
