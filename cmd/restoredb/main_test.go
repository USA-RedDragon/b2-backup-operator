package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/b2-backup-operator/cmd/restoredb/sqlite"
	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/testutil"
)

const (
	argDryRun     = "--dry-run"
	testDB        = "app"
	engineMariaDB = "mariadb"
)

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
			args: []string{"cnpg", argDryRun, "-H", "db", "-d", testDB, "-u", "postgres"},
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
			got, err := testutil.RunGolden(t, New(), tt.args...)
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

	got, err := testutil.RunGolden(t, New(), "cnpg", argDryRun, "--secret-mount", dir)
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

	got, err := testutil.RunGolden(t, New(), "cnpg", argDryRun, "--secret-mount", dir, "-H", "from-flag")
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
