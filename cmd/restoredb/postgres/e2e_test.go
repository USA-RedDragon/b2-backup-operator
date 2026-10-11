//go:build postgres

package postgres_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	pgPassword = "pg-pw"
	appDB      = "app"
)

var server string

// TestMain starts one Postgres server in Docker for the whole suite and copies
// a static restoredb build into it. Each restore runs that binary inside the
// container, next to psql and pg_restore, so stopping psql really stops it, as
// in the backup image. The suite needs nothing but Docker and Go.
// POSTGRES_IMAGE overrides the image.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	image := os.Getenv("POSTGRES_IMAGE")
	if image == "" {
		image = "postgres:18-alpine"
	}
	bin, err := os.MkdirTemp("", "b2bk-restoredb")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(bin) }()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "restoredb"), "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build restoredb: %v\n%s", err, out)
		return 1
	}

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	server = "b2bk-restoredb-pg-e2e-" + hex.EncodeToString(suffix)
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", server,
		"-e", "POSTGRES_PASSWORD="+pgPassword, image).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "start %s: %v\n%s", image, err, out)
		return 1
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", "-v", server).Run() }()

	ready := false
	for range 120 {
		if exec.Command("docker", "exec", "-e", "PGPASSWORD="+pgPassword, server,
			"psql", "-h127.0.0.1", "-Upostgres", "-c", "SELECT 1").Run() == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		fmt.Fprintln(os.Stderr, "the Postgres server did not come up")
		return 1
	}
	if out, err := exec.Command("docker", "cp", filepath.Join(bin, "restoredb"),
		server+":/usr/local/bin/restoredb").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "copy restoredb: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func docker(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", slices.Concat([]string{"exec", "-i", "-e", "PGPASSWORD=" + pgPassword, server},
		args)...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, errOut.String())
	}
	return out.String()
}

func sql(t *testing.T, db, stmts string) string {
	t.Helper()
	return docker(t, stmts, "psql", "-h127.0.0.1", "-Upostgres", "-d", db, "-v", "ON_ERROR_STOP=1",
		"--no-psqlrc", "-qAt")
}

// dump takes a dump of app with pg_dump and any extra flags.
func dump(t *testing.T, args ...string) string {
	t.Helper()
	return docker(t, "", slices.Concat([]string{"pg_dump", "-h127.0.0.1", "-Upostgres"}, args, []string{appDB})...)
}

// fingerprint is a deterministic plain dump of app: schema, rows, large
// objects, constraints and indexes.
func fingerprint(t *testing.T) string {
	t.Helper()
	return dump(t, "--restrict-key=fingerprint")
}

const seedSQL = `
CREATE TABLE parent (id int PRIMARY KEY);
CREATE TABLE t (id int PRIMARY KEY, pid int REFERENCES parent (id), v text);
CREATE INDEX t_v ON t (v);
INSERT INTO parent SELECT g FROM generate_series(1, 10) g;
INSERT INTO t SELECT g, g % 10 + 1, 'row ' || g FROM generate_series(1, 5000) g;
`

// seed recreates app, fills it, and returns a dump taken with args and the
// fingerprint of the state it holds. app is then changed, so a restore that
// does nothing is told apart from one that worked.
func seed(t *testing.T, extra string, args ...string) (backup, backupState string) {
	t.Helper()
	sql(t, "postgres", "DROP DATABASE IF EXISTS "+appDB+" WITH (FORCE); CREATE DATABASE "+appDB)
	sql(t, appDB, seedSQL+extra)
	backupState = fingerprint(t)
	backup = dump(t, args...)
	sql(t, appDB, "UPDATE t SET v = 'changed'; DELETE FROM t WHERE id > 4000; INSERT INTO parent VALUES (11);")
	return backup, backupState
}

// restore pipes input into restoredb postgres inside the container. The input
// ends only after a pause, so psql has run every statement it was given by the
// time restoredb sees the end, as it would in a large dump.
func restore(t *testing.T, input []byte, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("docker", slices.Concat([]string{"exec", "-i", server, "restoredb", "postgres",
		"-H", "127.0.0.1", "-u", "postgres", "-p", pgPassword, "-d", appDB}, args)...)
	cmd.Stdin = io.MultiReader(bytes.NewReader(input), pause(2*time.Second))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	t.Logf("restoredb output:\n%s", out.String())
	return out.String(), err
}

type pause time.Duration

func (p pause) Read([]byte) (int, error) {
	time.Sleep(time.Duration(p))
	return 0, io.EOF
}

// refused runs a restore that must fail and leave app exactly as it was.
func refused(t *testing.T, input []byte, args ...string) string {
	t.Helper()
	before := fingerprint(t)
	out, err := restore(t, input, args...)
	if err == nil {
		t.Fatal("the restore succeeded")
	}
	if after := fingerprint(t); after != before {
		t.Errorf("the database changed: %s", firstDiff(before, after))
	}
	return out
}

func restored(t *testing.T, input []byte, want string) {
	t.Helper()
	if _, err := restore(t, input); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := fingerprint(t); got != want {
		t.Errorf("app does not match the backup: %s", firstDiff(want, got))
	}
}

func TestPostgresRestoresAPlainDump(t *testing.T) {
	backup, want := seed(t, "", "--clean", "--if-exists")
	restored(t, []byte(backup), want)
}

func TestPostgresRestoresACustomDump(t *testing.T) {
	backup, want := seed(t, "", "-Fc")
	restored(t, []byte(backup), want)
}

func TestPostgresRefusesATruncatedPlainDump(t *testing.T) {
	backup, _ := seed(t, "", "--clean", "--if-exists")
	cut, _, ok := strings.Cut(backup, "ALTER TABLE ONLY public.t")
	if !ok {
		t.Fatal("the dump adds no constraints to t")
	}
	refused(t, []byte(cut))
}

// firstDiff describes the first line where two fingerprints differ.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(w), len(g)) {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("line %d is %q, want %q (%d lines, want %d)", i+1, gl, wl, len(g), len(w))
		}
	}
	return "no difference"
}
