package mariadb

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	weird       = "we`ird"
	createTable = "CREATE TABLE `t` (i int);\n"
)

func TestNames(t *testing.T) {
	t.Parallel()
	staging, old, err := Names("app")
	if err != nil || staging != "app__b2bk_restore" || old != "app__b2bk_old" {
		t.Errorf("Names(app) = %q, %q, %v", staging, old, err)
	}
	if _, _, err := Names(strings.Repeat("x", 50)); err != nil {
		t.Errorf("a 50 character name leaves room for the suffix: %v", err)
	}
	if _, _, err := Names(strings.Repeat("x", 51)); !errors.Is(err, ErrNameTooLong) {
		t.Errorf("err = %v, want ErrNameTooLong", err)
	}
	if _, _, err := Names(""); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("err = %v, want ErrNoDatabase", err)
	}
}

func TestQuoteIdent(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		testLive:  "`app`",
		weird:     "`we``ird`",
		"sp ace":  "`sp ace`",
		"`":       "````",
		"a.b":     "`a.b`",
		"tab\tin": "`tab\tin`",
	} {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLiteralIsHex(t *testing.T) {
	t.Parallel()
	if got := literal("a'b\\"); got != "X'6127625c'" {
		t.Errorf("literal = %s", got)
	}
}

func TestRenameStatement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		current, incoming []string
		want              string
	}{
		{
			name:     "tables on both sides",
			current:  []string{"b", "a"},
			incoming: []string{"a", "b"},
			want: "RENAME TABLE `app`.`a` TO `old`.`a`, `stg`.`a` TO `app`.`a`, " +
				"`app`.`b` TO `old`.`b`, `stg`.`b` TO `app`.`b`",
		},
		{
			name:     "a live-only table moves out, a staging-only table moves in",
			current:  []string{"extra", "keep"},
			incoming: []string{"gone", "keep"},
			want: "RENAME TABLE `app`.`extra` TO `old`.`extra`, `stg`.`gone` TO `app`.`gone`, " +
				"`app`.`keep` TO `old`.`keep`, `stg`.`keep` TO `app`.`keep`",
		},
		{
			name:     "an empty live database",
			incoming: []string{"t"},
			want:     "RENAME TABLE `stg`.`t` TO `app`.`t`",
		},
		{
			name:    "an empty dump empties the live database",
			current: []string{"t"},
			want:    "RENAME TABLE `app`.`t` TO `old`.`t`",
		},
		{
			name: "nothing to move",
			want: "",
		},
		{
			name:     "quoted names",
			current:  []string{weird},
			incoming: []string{weird},
			want:     "RENAME TABLE `app`.`we``ird` TO `old`.`we``ird`, `stg`.`we``ird` TO `app`.`we``ird`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RenameStatement("app", "stg", "old", tt.current, tt.incoming); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

// Each table is moved out of live before its namesake moves in, which is the
// order MariaDB needs within one RENAME TABLE.
func TestRenameStatementMovesOutBeforeIn(t *testing.T) {
	t.Parallel()
	got := RenameStatement("app", "stg", "old", []string{"t"}, []string{"t"})
	if strings.Index(got, "`app`.`t` TO") > strings.Index(got, "TO `app`.`t`") {
		t.Errorf("the live table must move out first: %s", got)
	}
}

func TestDumpComplete(t *testing.T) {
	t.Parallel()
	body := "-- MariaDB dump 10.20\nINSERT INTO `t` VALUES (1);\n/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;\n\n"
	tests := []struct {
		name string
		dump string
		want bool
	}{
		{"--skip-dump-date, as dumpdb runs it", body + "-- Dump completed\n", true},
		{"with the date", body + "-- Dump completed on 2026-10-11  9:15:12\n", true},
		{"trailing blank lines", body + "-- Dump completed\n\n\n", true},
		{"CRLF", strings.ReplaceAll(body+"-- Dump completed\n", "\n", "\r\n"), true},
		{"no trailing newline", body + "-- Dump completed", true},
		{"cut before the trailer", body, false},
		{"cut inside the trailer", body + "-- Dump compl", false},
		{"cut inside a row", body + "INSERT INTO `t` VALUES (1,'-- Dump completed", false},
		{"the trailer, then more", body + "-- Dump completed\nINSERT INTO `t` VALUES (2);\n", false},
		{"a lookalike", body + "-- Dump completedness\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		if got := DumpComplete([]byte(tt.dump)); got != tt.want {
			t.Errorf("%s: DumpComplete = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		line string
		kind lineKind
		arg  string
	}{
		{"USE `app`;\n", lineDatabase, testLive},
		{"use `My``Db`;", lineDatabase, "My`Db"},
		{"CREATE DATABASE /*!32312 IF NOT EXISTS*/ `app` /*!40100 DEFAULT CHARACTER SET utf8mb4 */;\n",
			lineDatabase, testLive},
		{"CREATE DATABASE `app`;", lineDatabase, testLive},
		{"CREATE SCHEMA IF NOT EXISTS `app`;", lineDatabase, testLive},
		{"/*!40000 DROP DATABASE IF EXISTS `app`*/;\n", lineDatabase, testLive},
		{"DROP SCHEMA `app`;", lineDatabase, testLive},
		{"USE app;", lineUnsupported, "USE app;"},
		{"ALTER DATABASE `app` CHARACTER SET latin1 ;", lineUnsupported, "ALTER DATABASE `app` CHARACTER SET latin1 ;"},
		{"/*!50003 ALTER DATABASE `app` CHARACTER SET latin1 */;", lineUnsupported,
			"/*!50003 ALTER DATABASE `app` CHARACTER SET latin1 */;"},
		{"CREATE OR REPLACE DATABASE `app`;", lineUnsupported, "CREATE OR REPLACE DATABASE `app`;"},
		{"DELIMITER ;;\n", lineRefused, "triggers, stored routines or events"},
		{"/*!50001 CREATE ALGORITHM=UNDEFINED */\n", lineRefused, "views"},
		{"/*!50001 CREATE TABLE `v` (\n", lineRefused, "views"},
		{"CREATE TABLE `t` (\n", lineObject, ""},
		{"DROP TABLE IF EXISTS `t`;\n", lineObject, ""},
		{"INSERT INTO `t` VALUES (1);\n", lineObject, ""},
		{"/*!40101 SET NAMES utf8mb4 */;\n", linePass, ""},
		{"/*M!999999\\- enable the sandbox mode */ \n", linePass, ""},
		{"-- Current Database: `app`\n", linePass, ""},
		{"  `USE` int(11) DEFAULT NULL,\n", linePass, ""},
		{"LOCK TABLES `t` WRITE;\n", linePass, ""},
	}
	for _, tt := range tests {
		kind, arg := classify(tt.line)
		if kind != tt.kind || arg != tt.arg {
			t.Errorf("classify(%q) = %d, %q, want %d, %q", tt.line, kind, arg, tt.kind, tt.arg)
		}
	}
}

func filterString(t *testing.T, dump string) (string, *dumpFilter, error) {
	t.Helper()
	var out strings.Builder
	f := &dumpFilter{}
	err := f.copy(&out, bufio.NewReaderSize(strings.NewReader(dump), 64))
	return out.String(), f, err
}

func TestFilterLeavesOutDatabaseLines(t *testing.T) {
	t.Parallel()
	dump := "/*!40000 DROP DATABASE IF EXISTS `app`*/;\n" +
		"CREATE DATABASE /*!32312 IF NOT EXISTS*/ `app`;\n" +
		"USE `app`;\n" +
		"CREATE TABLE `t` (\n  `id` int\n);\n" +
		"INSERT INTO `t` VALUES (1),(2),(3),(4),(5),(6),(7),(8),(9);\n" +
		"USE `app`;\n" +
		"-- Dump completed\n"
	got, f, err := filterString(t, dump)
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE TABLE `t` (\n  `id` int\n);\n" +
		"INSERT INTO `t` VALUES (1),(2),(3),(4),(5),(6),(7),(8),(9);\n" +
		"-- Dump completed\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if f.database != testLive {
		t.Errorf("database = %q", f.database)
	}
	if !DumpComplete(f.tail) {
		t.Errorf("tail = %q", f.tail)
	}
}

// A line longer than the reader's buffer is passed on whole, and text inside
// it that looks like a statement is data.
func TestFilterPassesLongLines(t *testing.T) {
	t.Parallel()
	dump := "INSERT INTO `t` VALUES ('" + strings.Repeat("x", 100) + "\\nUSE `other`;" +
		strings.Repeat("USE `x`;", 20) + "');\n-- Dump completed\n"
	got, f, err := filterString(t, dump)
	if err != nil || got != dump {
		t.Errorf("got %q, %v", got, err)
	}
	if f.database != "" {
		t.Errorf("database = %q", f.database)
	}
}

func TestFilterRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, dump string
		want       error
		passed     string
	}{
		{
			name: "two databases",
			dump: "USE `a`;\nCREATE TABLE `t` (i int);\nUSE `b`;\nCREATE TABLE `u` (i int);\n",
			want: ErrMultipleDatabases, passed: createTable,
		},
		{
			name: "a database named after the first table",
			dump: "CREATE TABLE `t` (i int);\nUSE `a`;\n",
			want: ErrMultipleDatabases, passed: createTable,
		},
		{
			name: "a trigger",
			dump: "CREATE TABLE `t` (i int);\nDELIMITER ;;\n/*!50003 CREATE*/ /*!50003 TRIGGER x */;;\n",
			want: ErrUnsupportedObjects, passed: createTable,
		},
		{
			name: "a view",
			dump: "/*!50001 DROP VIEW IF EXISTS `v`*/;\n",
			want: ErrUnsupportedObjects,
		},
		{
			name: "ALTER DATABASE",
			dump: "ALTER DATABASE `app` CHARACTER SET latin1 ;\n",
			want: ErrUnsupportedStatement,
		},
		{
			name: "an unquoted USE",
			dump: "USE app;\n",
			want: ErrUnsupportedStatement,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := filterString(t, tt.dump)
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if got != tt.passed {
				t.Errorf("passed on %q, want %q", got, tt.passed)
			}
		})
	}
}

func TestParseBatch(t *testing.T) {
	t.Parallel()
	rows := parseBatch("a\tb\\tc\nd\\\\e\tf\\ng\n\n")
	if len(rows) != 2 || rows[0][0] != "a" || rows[0][1] != "b\tc" || rows[1][0] != `d\e` || rows[1][1] != "f\ng" {
		t.Errorf("rows = %q", rows)
	}
	if rows := parseBatch(""); len(rows) != 0 {
		t.Errorf("empty output gave %q", rows)
	}
}

func TestClientError(t *testing.T) {
	t.Parallel()
	stderr := "WARNING: option --ssl-verify-server-cert is disabled\nERROR 1044 (42000): Access denied\n"
	if got := clientError(stderr); got != "ERROR 1044 (42000): Access denied" {
		t.Errorf("got %q", got)
	}
	if got := clientError("something else\n"); got != "something else" {
		t.Errorf("got %q", got)
	}
}

func TestClientArgs(t *testing.T) {
	t.Parallel()
	c := &client{host: "db", port: "3306", user: "root", extra: []string{"--skip-ssl"}}
	got := strings.Join(c.args("app__b2bk_restore"), " ")
	if want := "--host=db --port=3306 --user=root app__b2bk_restore --skip-ssl"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// SQL goes to the client on stdin: a swap of many tables is longer than Linux
// allows a single argument.
func TestClientSendsSQLOnStdin(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >" + bin + "/args\ncat >" + bin + "/stdin\n"
	if err := os.WriteFile(filepath.Join(bin, "mariadb"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stmt := "RENAME TABLE " + strings.Repeat("`app`.`a_long_table_name` TO `app__b2bk_old`.`a_long_table_name`, ", 4000)
	c := &client{host: "db", port: "3306", user: "root", stdout: io.Discard, stderr: io.Discard}
	if err := c.execute(context.Background(), stmt); err != nil {
		t.Fatalf("execute: %v", err)
	}
	args, _ := os.ReadFile(filepath.Join(bin, "args"))
	stdin, _ := os.ReadFile(filepath.Join(bin, "stdin"))
	if strings.Contains(string(args), "RENAME") {
		t.Errorf("the SQL is on the command line: %.200s", args)
	}
	if !strings.Contains(string(stdin), stmt) {
		t.Errorf("stdin holds %d bytes, not the statement", len(stdin))
	}
}
