package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/b2-backup-operator/cmd/restoredb/postgres"
	"github.com/USA-RedDragon/b2-backup-operator/cmd/restoredb/sqlite"
	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/testutil"
	"github.com/spf13/cobra"
)

const (
	argDryRun     = "--dry-run"
	testDB        = "app"
	engineMariaDB = "mariadb"
	engineCNPG    = "cnpg"
	customDump    = "PGDMP\x01\x10\x00\x04\x08\x01\x01\x01"
	plainDump     = "--\n-- PostgreSQL database dump\n--\n\\restrict abc\nSET statement_timeout = 0;\n"
	plainTrailer  = "--\n-- PostgreSQL database dump complete\n--\n\n\\unrestrict abc\n\n"
)

func withStdin(s string) *cobra.Command {
	cmd := New()
	cmd.SetIn(strings.NewReader(s))
	return cmd
}

func TestPostgresPicksRestoreToolFromDumpFormat(t *testing.T) {
	const conn = " --host=db --port=5432 --username=app --dbname=app"
	tests := []struct {
		name    string
		stdin   string
		extra   []string
		want    string
		wantErr error
	}{
		{
			name:  "custom format uses pg_restore",
			stdin: customDump,
			want:  "+ pg_restore --clean --if-exists --no-owner --single-transaction" + conn + "\n",
		},
		{
			name:  "plain sql uses psql",
			stdin: plainDump,
			want:  "+ psql -v ON_ERROR_STOP=1 --single-transaction --no-psqlrc --quiet" + conn + "\n",
		},
		{
			name:  "input shorter than the magic is plain sql",
			stdin: "PGD",
			want:  "+ psql -v ON_ERROR_STOP=1 --single-transaction --no-psqlrc --quiet" + conn + "\n",
		},
		{
			name:  "lowercase magic is plain sql",
			stdin: "pgdmp",
			want:  "+ psql -v ON_ERROR_STOP=1 --single-transaction --no-psqlrc --quiet" + conn + "\n",
		},
		{
			name:  "psql passthrough after separator",
			stdin: plainDump,
			extra: []string{"--", "--echo-errors"},
			want:  "+ psql -v ON_ERROR_STOP=1 --single-transaction --no-psqlrc --quiet" + conn + " --echo-errors\n",
		},
		{
			name:    "empty input is refused",
			stdin:   "",
			wantErr: postgres.ErrEmptyDump,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.ClearEnv(t)
			args := append([]string{engineCNPG, argDryRun, "-H", "db", "-d", testDB, "-u", testDB}, tt.extra...)
			got, err := testutil.RunGolden(t, withStdin(tt.stdin), args...)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				if got != "" {
					t.Errorf("nothing should be traced, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got != tt.want {
				t.Errorf("argv mismatch\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestPostgresStreamsWholeDumpToRestoreTool(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"psql", "pg_restore"} {
		script := "#!/bin/sh\nprintf '%s:' " + name + "\nexec cat\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name  string
		stdin string
		tool  string
	}{
		{name: "custom", stdin: customDump + strings.Repeat("x", 1<<16), tool: "pg_restore"},
		{name: "plain", stdin: plainDump + strings.Repeat("SELECT 1;\n", 1<<13) + plainTrailer, tool: "psql"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.ClearEnv(t)
			var stdout strings.Builder
			cmd := withStdin(tt.stdin)
			cmd.SetArgs([]string{engineCNPG, "-H", "db"})
			cmd.SetOut(&stdout)
			cmd.SetErr(io.Discard)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if want := tt.tool + ":" + tt.stdin; stdout.String() != want {
				t.Errorf("%s got %d bytes, want %d starting %q", tt.tool, stdout.Len(), len(want), want[:16])
			}
		})
	}
}

func TestGoldenArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "postgres defaults",
			args: []string{"postgres", argDryRun},
			want: "+ pg_restore --clean --if-exists --no-owner --single-transaction" +
				" --host= --port=5432 --username= --dbname=\n",
		},
		{
			name: "cnpg alias matches postgres",
			args: []string{engineCNPG, argDryRun, "-H", "db", "-d", testDB, "-u", "postgres"},
			want: "+ pg_restore --clean --if-exists --no-owner --single-transaction" +
				" --host=db --port=5432 --username=postgres --dbname=app\n",
		},
		{
			name: engineMariaDB,
			args: []string{engineMariaDB, argDryRun, "-H", "db", "-d", testDB, "-u", "root"},
			want: "+ mariadb --host=db --port=3306 --user=root app\n",
		},
		{
			name: "mariadb passthrough after separator",
			args: []string{engineMariaDB, argDryRun, "-H", "db", "-d", testDB, "-u", "root", "--", "--skip-ssl"},
			want: "+ mariadb --host=db --port=3306 --user=root app --skip-ssl\n",
		},
		{
			name: "mongodb redacts the password",
			args: []string{"mongodb", argDryRun, "-H", "h", "-d", testDB, "-u", "u", "-p", "hunter2"},
			want: "+ mongorestore --archive --drop --authenticationDatabase=" +
				" --host=h --port=27017 --username=u --password=*** --db=app\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.ClearEnv(t)
			got, err := testutil.RunGolden(t, withStdin(customDump), tt.args...)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got != tt.want {
				t.Errorf("argv mismatch\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// The operator mounts the CNPG Secret at dumpdb's fixed default path and passes
// no DB_* env, so restore has to resolve every connection detail from files.
func TestPostgresResolvesFromSecretMount(t *testing.T) {
	testutil.ClearEnv(t)

	dir := t.TempDir()
	for name, content := range map[string]string{
		"host":     "postgresql-rw",
		"port":     "5432",
		"dbname":   testDB,
		"username": testDB,
		"password": "hunter2\n", // trailing newline must be trimmed
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := testutil.RunGolden(t, withStdin(customDump), engineCNPG, argDryRun, "--secret-mount", dir)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	want := "+ pg_restore --clean --if-exists --no-owner --single-transaction" +
		" --host=postgresql-rw --port=5432 --username=app --dbname=app\n"
	if got != want {
		t.Errorf("argv mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestExplicitFlagBeatsSecretMount(t *testing.T) {
	testutil.ClearEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := testutil.RunGolden(t, withStdin(customDump),
		engineCNPG, argDryRun, "--secret-mount", dir, "-H", "from-flag")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := "--host=from-flag"; !strings.Contains(got, want) {
		t.Errorf("expected %q in %q", want, got)
	}
}

func TestSqliteRefusesToOverwriteWithoutForce(t *testing.T) {
	testutil.ClearEnv(t)

	path := filepath.Join(t.TempDir(), "app.db")
	if err := os.WriteFile(path, []byte("existing data"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := testutil.RunGolden(t, New(), "sqlite", path, argDryRun); !errors.Is(err, sqlite.ErrFileExists) {
		t.Fatalf("expected ErrFileExists, got %v", err)
	}

	// The guarded file must survive the refusal.
	if data, err := os.ReadFile(path); err != nil || string(data) != "existing data" {
		t.Errorf("file was modified: %q, %v", data, err)
	}
}

func TestSqliteRestoresToMissingFile(t *testing.T) {
	testutil.ClearEnv(t)

	path := filepath.Join(t.TempDir(), "app.db")
	got, err := testutil.RunGolden(t, New(), "sqlite", path, argDryRun)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if want := "+ sqlite3 -bail " + path + "\n"; got != want {
		t.Errorf("argv mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestPostgresNeverLetsPsqlCommitATruncatedPlainDump(t *testing.T) {
	bin := t.TempDir()
	status := filepath.Join(bin, "status")
	script := "#!/bin/sh\ncat >/dev/null\necho committed >" + status + "\n"
	if err := os.WriteFile(filepath.Join(bin, "psql"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name      string
		stdin     string
		committed bool
	}{
		{"cut short", plainDump + strings.Repeat("INSERT INTO t VALUES (1);\n", 1<<12), false},
		{"complete", plainDump + strings.Repeat("INSERT INTO t VALUES (1);\n", 1<<12) + plainTrailer, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutil.ClearEnv(t)
			_ = os.Remove(status)
			cmd := withStdin(tt.stdin)
			cmd.SetArgs([]string{engineCNPG, "-H", "db"})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			_, statErr := os.Stat(status)
			if committed := statErr == nil; committed != tt.committed {
				t.Errorf("psql reached the end of its input = %t, want %t (err %v)", committed, tt.committed, err)
			}
			if tt.committed != (err == nil) {
				t.Errorf("err = %v", err)
			}
		})
	}
}
