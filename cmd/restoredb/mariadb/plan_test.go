package mariadb

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	weird       = "we`ird"
	testUser    = "root"
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
			want: "RENAME TABLE `app`.`a` TO `old`.`a`, `app`.`b` TO `old`.`b`, " +
				"`stg`.`a` TO `app`.`a`, `stg`.`b` TO `app`.`b`",
		},
		{
			name:     "a live-only table moves out, a staging-only table moves in",
			current:  []string{"extra", "keep"},
			incoming: []string{"gone", "keep"},
			want: "RENAME TABLE `app`.`extra` TO `old`.`extra`, `app`.`keep` TO `old`.`keep`, " +
				"`stg`.`gone` TO `app`.`gone`, `stg`.`keep` TO `app`.`keep`",
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

// Every live table is moved out before any staged table moves in: MariaDB
// applies the parts in order, and a foreign key name must stay unique in live.
func TestRenameStatementMovesOutBeforeIn(t *testing.T) {
	t.Parallel()
	got := RenameStatement("app", "stg", "old", []string{"b", "a"}, []string{"a", "c"})
	want := "RENAME TABLE `app`.`a` TO `old`.`a`, `app`.`b` TO `old`.`b`, `stg`.`a` TO `app`.`a`, " +
		"`stg`.`c` TO `app`.`c`"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
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
	const (
		createFK = "CREATE TABLE `t` (\n  `id` int,\n" +
			"  CONSTRAINT `fk` FOREIGN KEY (`id`) REFERENCES `other`.`p` (`id`)\n)"
		otherT = "`other`.`t`"
		oT     = "`o`.`t`"
	)
	tests := []struct {
		stmt string
		kind stmtKind
		arg  string
	}{
		{"USE `app`", stmtDatabase, testLive},
		{"use `My``Db`", stmtDatabase, "My`Db"},
		{"CREATE DATABASE /*!32312 IF NOT EXISTS*/ `app` /*!40100 DEFAULT CHARACTER SET utf8mb4 */",
			stmtDatabase, testLive},
		{"CREATE DATABASE `app`", stmtDatabase, testLive},
		{"CREATE SCHEMA IF NOT EXISTS `app`", stmtDatabase, testLive},
		{"/*!40000 DROP DATABASE IF EXISTS `app`*/", stmtDatabase, testLive},
		{"DROP SCHEMA `app`", stmtDatabase, testLive},
		{"USE app", stmtUnsupported, "USE app"},
		{"ALTER DATABASE `app` CHARACTER SET latin1 ", stmtUnsupported, "ALTER DATABASE `app` CHARACTER SET latin1 "},
		{"/*!50003 ALTER DATABASE `app` CHARACTER SET latin1 */", stmtUnsupported,
			"/*!50003 ALTER DATABASE `app` CHARACTER SET latin1 */"},
		{"CREATE OR REPLACE DATABASE `app`", stmtUnsupported, "CREATE OR REPLACE DATABASE `app`"},
		{"DELIMITER ", stmtRefused, objectsTriggers},
		{"  delimiter //\nCREATE TRIGGER x", stmtRefused, objectsTriggers},
		{"/*!50001 CREATE ALGORITHM=UNDEFINED */", stmtRefused, objectsViews},
		{"/*!50001 CREATE TABLE `v` (\n", stmtRefused, objectsViews},
		{"CREATE DEFINER=`root`@`%` PROCEDURE `p`() SELECT 1", stmtRefused, objectsTriggers},
		{"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER `x` BEFORE INSERT ON `t`",
			stmtRefused, objectsTriggers},
		{"CREATE OR REPLACE VIEW `v` AS SELECT 1", stmtRefused, objectsViews},
		{"CREATE TABLE `t` (\n", stmtObject, ""},
		{createFK, stmtObject, ""},
		{"CREATE SEQUENCE `s` start with 1 minvalue 1 maxvalue 9223372036854775806 increment by 1 cache 1000 " +
			"nocycle ENGINE=InnoDB", stmtObject, ""},
		{"DROP TABLE IF EXISTS `t`", stmtObject, ""},
		{"INSERT INTO `t` VALUES (1)", stmtObject, ""},
		{"/*!40101 SET NAMES utf8mb4 */", stmtPass, ""},
		{"/*M!999999 enable the sandbox mode */ \n/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */",
			stmtPass, ""},
		{"/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */", stmtPass, ""},
		{"LOCK TABLES `t` WRITE", stmtPass, ""},
		{"/*!40000 ALTER TABLE `t` DISABLE KEYS */", stmtPass, ""},
		{"SELECT SETVAL(`s`, 1001, 0)", stmtPass, ""},
		{"START TRANSACTION", stmtPass, ""},
		{"INSERT INTO `other`.`t` VALUES (1)", stmtOtherDatabase, otherT},
		{"insert ignore into other.t values (1)", stmtOtherDatabase, otherT},
		{"REPLACE INTO `app`.`t` VALUES (1)", stmtOtherDatabase, "`app`.`t`"},
		{"/*!40000 ALTER TABLE `other`.`t` DISABLE KEYS */", stmtOtherDatabase, otherT},
		{"LOCK TABLES `t` WRITE, `o`.`u` READ", stmtOtherDatabase, "`o`.`u`"},
		{"DROP TABLE IF EXISTS `t`, `o`.`u`", stmtOtherDatabase, "`o`.`u`"},
		{"RENAME TABLE `t` TO `o`.`t`", stmtOtherDatabase, oT},
		{"ALTER TABLE `t` RENAME TO `o`.`t`", stmtOtherDatabase, oT},
		{"SELECT SETVAL(`o`.`s`, 5, 0)", stmtOtherDatabase, "`o`.`s`"},
		{"TRUNCATE TABLE `o`.`t`", stmtOtherDatabase, oT},
		{"DELETE FROM `o`.`t`", stmtOtherDatabase, oT},
		{"UPDATE `o`.`t` SET `a` = 1", stmtOtherDatabase, oT},
		{"CREATE INDEX `i` ON `o`.`t` (`a`)", stmtOtherDatabase, oT},
		{"CREATE TABLE `o`.`t` (`a` int)", stmtOtherDatabase, oT},
		{"CHANGE MASTER TO MASTER_LOG_FILE='x', MASTER_LOG_POS=4", stmtServer,
			"CHANGE MASTER TO MASTER_LOG_FILE='x', MASTER_LOG_POS=4"},
		{"SET GLOBAL gtid_slave_pos='0-1-2'", stmtServer, "SET GLOBAL gtid_slave_pos='0-1-2'"},
		{"/*!40101 SET @@GLOBAL.x=1 */", stmtServer, "/*!40101 SET @@GLOBAL.x=1 */"},
		{"SET @a = 1, global x = 2", stmtServer, "SET @a = 1, global x = 2"},
		{"SET PASSWORD = PASSWORD('x')", stmtServer, "SET PASSWORD = PASSWORD('x')"},
		{"SET STATEMENT max_statement_time=1 FOR DELETE FROM `o`.`t`", stmtServer,
			"SET STATEMENT max_statement_time=1 FOR DELETE FROM `o`.`t`"},
		{"START SLAVE", stmtServer, "START SLAVE"},
		{"STOP ALL SLAVES", stmtServer, "STOP ALL SLAVES"},
		{"GRANT ALL ON *.* TO `x`", stmtServer, "GRANT ALL ON *.* TO `x`"},
		{"CREATE USER `x`", stmtServer, "CREATE USER `x`"},
		{"EXECUTE IMMEDIATE 'DELETE FROM o.t'", stmtServer, "EXECUTE IMMEDIATE 'DELETE FROM o.t'"},
		{"PREPARE s FROM 'DELETE FROM o.t'", stmtServer, "PREPARE s FROM 'DELETE FROM o.t'"},
		{"CALL `o`.`p`()", stmtServer, "CALL `o`.`p`()"},
		{"LOAD DATA INFILE 'x' INTO TABLE `t`", stmtServer, "LOAD DATA INFILE 'x' INTO TABLE `t`"},
		{"SET SQL_MODE='ANSI_QUOTES'", stmtUnsupported,
			"an sql_mode the restore cannot read the dump under: SET SQL_MODE='ANSI_QUOTES'"},
		{"/*!40101 SET SQL_MODE='STRICT_ALL_TABLES,NO_BACKSLASH_ESCAPES' */", stmtUnsupported,
			"an sql_mode the restore cannot read the dump under: " +
				"/*!40101 SET SQL_MODE='STRICT_ALL_TABLES,NO_BACKSLASH_ESCAPES' */"},
	}
	for _, tt := range tests {
		kind, arg := classify(tt.stmt, true)
		if kind != tt.kind || arg != tt.arg {
			t.Errorf("classify(%q) = %d, %q, want %d, %q", tt.stmt, kind, arg, tt.kind, tt.arg)
		}
	}
	if kind, _ := classify("LOCK TABLES `t` WRITE, ", false); kind != stmtUnsupported {
		t.Errorf("an unfinished list statement is %d", kind)
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
	want := "\n\n\nCREATE TABLE `t` (\n  `id` int\n);\n" +
		"INSERT INTO `t` VALUES (1),(2),(3),(4),(5),(6),(7),(8),(9);\n" +
		"\n-- Dump completed\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if f.database != testLive {
		t.Errorf("database = %q", f.database)
	}
	if !f.complete() {
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
			want: ErrMultipleDatabases, passed: "\n" + createTable,
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
		{
			name: "an indented DELIMITER",
			dump: createTable + "  DELIMITER ;;\n",
			want: ErrUnsupportedObjects, passed: createTable,
		},
		{
			name: "a second statement on a line",
			dump: createTable + "SELECT 1; INSERT INTO `other`.`t` VALUES (1);\n",
			want: ErrOtherDatabase, passed: createTable,
		},
		{
			name: "a statement split over lines",
			dump: createTable + "INSERT INTO\n`other`\n.`t` VALUES (1);\n",
			want: ErrOtherDatabase, passed: createTable,
		},
		{
			name: "a comment between the words",
			dump: createTable + "SET/* x */GLOBAL max_connections = 1;\n",
			want: ErrServerStatement, passed: createTable,
		},
		{
			name: "replication",
			dump: createTable + "CHANGE MASTER TO MASTER_LOG_FILE='mysql-bin.000001', MASTER_LOG_POS=4;\n",
			want: ErrServerStatement, passed: createTable,
		},
		{
			name: "an executable comment left open on an earlier line",
			dump: createTable + "/*!40000\n/* */ GRANT ALL ON `mysql`.* TO 'z'@'%'; /* */\nSELECT 1;\n",
			want: ErrServerStatement, passed: createTable,
		},
		{
			name: "a table named past the part read before passing it on",
			dump: createTable + "INSERT" + strings.Repeat(" ", 4200) + "INTO `other`.`t` VALUES (1);\n",
			want: ErrUnsupportedStatement, passed: createTable,
		},
		{
			name: "a table named after a long comment",
			dump: createTable + "INSERT /*" + strings.Repeat("x", 70000) + "*/ INTO `other`.`t` VALUES (1);\n",
			want: ErrUnsupportedStatement, passed: createTable,
		},
		{
			name: "a file written by the server",
			dump: createTable + "SELECT * FROM `t` INTO OUTFILE '/tmp/x';\n",
			want: ErrServerStatement, passed: createTable,
		},
		{
			name: "a client command",
			dump: createTable + "\\u other\n",
			want: ErrClientCommand, passed: createTable,
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
	c := &client{host: "db", port: "3306", user: testUser, extra: []string{"--skip-ssl"}}
	got := strings.Join(c.args("--batch"), " ")
	if want := "--host=db --port=3306 --user=root --batch --skip-ssl"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// fakeClient puts a mariadb on PATH that saves its arguments and stdin in dir,
// and returns a client that runs it.
func fakeClient(t *testing.T) (c *client, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >" + dir + "/args\ncat >" + dir + "/stdin\n"
	if err := os.WriteFile(filepath.Join(dir, "mariadb"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &client{host: "db", port: "3306", user: testUser, stdout: io.Discard, stderr: io.Discard}, dir
}

func saved(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// SQL goes to the client on stdin: a swap of many tables is longer than Linux
// allows a single argument.
func TestClientSendsSQLOnStdin(t *testing.T) {
	c, dir := fakeClient(t)
	stmt := "RENAME TABLE " + strings.Repeat("`app`.`a_long_table_name` TO `app__b2bk_old`.`a_long_table_name`, ", 4000)
	if err := c.execute(context.Background(), stmt); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if args := saved(t, dir, "args"); strings.Contains(args, "RENAME") {
		t.Errorf("the SQL is on the command line: %.200s", args)
	}
	if stdin := saved(t, dir, "stdin"); !strings.Contains(stdin, stmt) {
		t.Errorf("stdin holds %d bytes, not the statement", len(stdin))
	}
}

// A database named like an option is passed as --database=, never on its own.
func TestClientLoadNamesTheDatabaseAsAnOption(t *testing.T) {
	c, dir := fakeClient(t)
	if err := c.load(context.Background(), "-app__b2bk_restore", func(io.Writer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(saved(t, dir, "args")), "\n")
	if !slices.Contains(args, "--database=-app__b2bk_restore") || slices.Contains(args, "-app__b2bk_restore") {
		t.Errorf("args = %q", args)
	}
}

// A name qualified with a database is refused wherever it is in a statement,
// except in CREATE TABLE, where mariadb-dump writes them for a foreign key to
// another database and for a sequence default, and nothing runs.
func TestFilterRefusesNamesInOtherDatabases(t *testing.T) {
	t.Parallel()
	rows := strings.Repeat("(1,'a.b -- `x`.`y`'),", 400)
	for _, stmt := range []string{
		"UPDATE `t`, `other`.`u` SET `other`.`u`.`x`=1",
		"DELETE `other`.`u` FROM `other`.`u`",
		"DELETE `t`, `u` FROM `t` JOIN `other`.`u`",
		"DELETE `t` FROM `other`.`t`",
		"ALTER TABLE `t` EXCHANGE PARTITION p0 WITH TABLE `other`.`u`",
		"INSERT INTO `t` SELECT * FROM `other`.`u`",
		"ALTER TABLE `t` RENAME `other`.`u`",
		"DO NEXTVAL(`other`.`s`)",
		"DO other.f()",
		"ANALYZE TABLE `other`.`u`",
		"SELECT `other` . `f`()",
		"SELECT `other`/* x */.`f`()",
		"INSERT INTO `t` VALUES " + rows + "(`other`.`f`())",
	} {
		got, _, err := filterString(t, createTable+stmt+";\n")
		if !errors.Is(err, ErrOtherDatabase) {
			t.Errorf("%.80q: err = %v", stmt, err)
		}
		rest := stmt[strings.LastIndex(stmt, "other")-1:]
		if !strings.HasPrefix(got, createTable) || strings.Contains(got, rest) {
			t.Errorf("%.80q: passed on %.200q", stmt, got)
		}
	}
	if _, _, err := filterString(t, "CREATE TABLE `t2` AS SELECT 1;\n"); !errors.Is(err, ErrUnsupportedStatement) {
		t.Errorf("CREATE TABLE ... SELECT: err = %v", err)
	}
	for _, stmt := range []string{
		"CREATE TABLE `d` (\n  `id` int(11) DEFAULT nextval(`app`.`s`),\n" +
			"  CONSTRAINT `fk` FOREIGN KEY (`id`) REFERENCES `other`.`p` (`id`)\n) ENGINE=InnoDB",
		"SET @@SESSION.SQL_LOG_BIN = 0",
		"/*!40101 SET @saved_cs_client     = @@character_set_client */",
		"INSERT INTO `a.b` VALUES (1.5,'a.b',0x2E,.5,1.e3,_binary 'x.y',X'2E')," + rows + "(2,NULL)",
		"DO SETVAL(`s`, 1001, 0)",
		"UPDATE `t`, `u` SET `x` = 1",
	} {
		if _, _, err := filterString(t, stmt+";\n"); err != nil {
			t.Errorf("%.80q: err = %v", stmt, err)
		}
	}
}

// A refused dump stops the client before it sees the end of its input: the
// client would take that as the end of the statement it was reading, and run
// it.
func TestClientLoadKillsTheClientWhenTheDumpIsRefused(t *testing.T) {
	c, dir := fakeClient(t)
	script := "#!/bin/sh\ndata=$(cat)\necho \"$data\" >" + dir + "/eof\n"
	if err := os.WriteFile(filepath.Join(dir, "mariadb"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	err := c.load(context.Background(), "app__b2bk_restore", func(w io.Writer) error {
		_, _ = io.WriteString(w, "INSERT INTO `t` VALUES (1)")
		return refused
	})
	if !errors.Is(err, refused) {
		t.Errorf("err = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, statErr := os.Stat(filepath.Join(dir, "eof")); statErr == nil {
		t.Error("the client read to the end of its input")
	}
}

// MariaDB reads a word led by a digit, such as 1db, as a name unless it is all
// digits, so 1db.t names a table in database 1db.
func TestFilterRefusesADatabaseNameLedByADigit(t *testing.T) {
	t.Parallel()
	for _, stmt := range []string{
		"INSERT INTO 1db.t VALUES (1)",
		"UPDATE `t` SET `c` = (SELECT c FROM 9x.t2)",
		"DO `other`.1f()",
		"DO 1db . `f`()",
	} {
		got, _, err := filterString(t, createTable+stmt+";\n")
		if !errors.Is(err, ErrOtherDatabase) || got != createTable {
			t.Errorf("%q: err = %v, passed on %q", stmt, err, got)
		}
	}
	if _, _, err := filterString(t, "INSERT INTO `t` VALUES (1.e3,1.5e3,0x1F,1e5,.5,1.,12.34);\n"); err != nil {
		t.Errorf("numbers: err = %v", err)
	}
}

// A MERGE table, or one whose engine reaches another server or database,
// writes past the staging database without naming a qualified table in the
// statement that writes.
func TestFilterRefusesEnginesThatReachOutside(t *testing.T) {
	t.Parallel()
	for _, stmt := range []string{
		"CREATE TABLE `m` (`id` INT) ENGINE=MRG_MyISAM UNION=(`other`.`t`) INSERT_METHOD=LAST",
		"CREATE TABLE `m` (`id` INT) ENGINE = merge UNION = (`t1`)",
		"CREATE TABLE `m` (`id` INT) UNION=(`t1`)",
		"CREATE TABLE `f` (`id` INT) ENGINE=FEDERATED CONNECTION='mysql://u@h/other/t'",
		"CREATE TABLE `c` (`id` INT) ENGINE=CONNECT TABLE_TYPE=MYSQL DBNAME='other' TABNAME='t'",
		"CREATE TABLE `s` (`id` INT) ENGINE=SPIDER COMMENT='wrapper \"mysql\", table \"t\"'",
		"CREATE TABLE `q` (`id` INT) ENGINE='MRG_MyISAM'",
		"CREATE TABLE `q` (`id` INT) ENGINE=`MERGE`",
		"ALTER TABLE `t` ENGINE=FEDERATED CONNECTION='mysql://u@h/other/t'",
		"/*!40000 ALTER TABLE `t` ENGINE=MERGE */",
	} {
		got, _, err := filterString(t, createTable+stmt+";\n")
		if !errors.Is(err, ErrOtherDatabase) || got != createTable {
			t.Errorf("%q: err = %v, passed on %q", stmt, err, got)
		}
	}
	for _, stmt := range []string{
		"CREATE TABLE `t2` (\n  `id` int(11) DEFAULT nextval(`app`.`s`),\n" +
			"  CONSTRAINT `fk` FOREIGN KEY (`id`) REFERENCES `other`.`p` (`id`)\n" +
			") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_uca1400_ai_ci",
		"CREATE TABLE `t3` (`id` int) ENGINE=MyISAM",
		"CREATE TABLE `t4` (`union` int, `connection` int) ENGINE=Aria PAGE_CHECKSUM=1",
		"CREATE SEQUENCE `s` start with 1 minvalue 1 maxvalue 9223372036854775806 increment by 1 cache 1000 " +
			"nocycle ENGINE=InnoDB",
	} {
		if _, _, err := filterString(t, stmt+";\n"); err != nil {
			t.Errorf("%q: err = %v", stmt, err)
		}
	}
}

// sql_mode decides how quotes read, so it may only be set to a literal the
// filter can check, or back to the mode a dump saved at its start.
func TestFilterFollowsSQLMode(t *testing.T) {
	t.Parallel()
	const save = "/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;\n"
	const restore = "/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;\n"
	for _, dump := range []string{
		"SET @m='ANSI_QUOTES';\nSET sql_mode=@m;\n",
		"SET sql_mode=X'414E53495F51554F544553';\n",
		"SET sql_mode=0x414E53495F51554F544553;\n",
		"SET SESSION sql_mode=CONCAT('AN', 'SI_QUOTES');\n",
		"SET @@session.sql_mode=@m;\n",
		"SET LOCAL sql_mode := @m;\n",
		"SET sql_mode=@never_saved;\n",
		save + "SET @OLD_SQL_MODE='ANSI_QUOTES';\n" + restore,
		save + "SELECT 'ANSI_QUOTES' INTO @OLD_SQL_MODE;\n" + restore,
		save + "DO @OLD_SQL_MODE:='ANSI_QUOTES';\n" + restore,
		save + "SET @a=(@OLD_SQL_MODE:='ANSI_QUOTES');\n" + restore,
		save + "INSERT INTO `t` VALUES (@`OLD_SQL_MODE`:='ANSI_QUOTES');\n" + restore,
	} {
		if _, _, err := filterString(t, createTable+dump); !errors.Is(err, ErrUnsupportedStatement) {
			t.Errorf("%q: err = %v", dump, err)
		}
	}
	for _, dump := range []string{
		save + goodDump + restore,
		"SET sql_mode=DEFAULT;\nSET @@SESSION.sql_mode='';\nSET sql_mode = 'STRICT_ALL_TABLES,NO_ZERO_DATE';\n",
		save + "SET @saved_cs_client = @@character_set_client;\n" + restore,
	} {
		if _, _, err := filterString(t, dump); err != nil {
			t.Errorf("%q: err = %v", dump, err)
		}
	}
}

// The last word of a statement is only finished by the semicolon that ends it,
// so an engine named there has to be seen too.
func TestFilterRefusesAnEngineNamedAtTheEndOfAStatement(t *testing.T) {
	t.Parallel()
	for _, stmt := range []string{
		"ALTER TABLE `t` ENGINE=MERGE",
		"ALTER TABLE `t` ENGINE = mrg_myisam",
		"CREATE TABLE `t` (`i` int) COMMENT='wrapper \"mysql\", srv \"s\", database \"other\"' ENGINE=SPIDER",
		"CREATE TABLE `t` (`i` int) ENGINE=FEDERATED",
		"CREATE TABLE `t` (`i` int) ENGINE=CONNECT",
	} {
		got, _, err := filterString(t, createTable+stmt+";\n")
		if !errors.Is(err, ErrOtherDatabase) || got != createTable {
			t.Errorf("%q: err = %v, passed on %q", stmt, err, got)
		}
	}
	for _, stmt := range []string{
		"ALTER TABLE `t` ENGINE=InnoDB",
		"CREATE TABLE `t` (`i` int) ENGINE=Aria",
	} {
		if _, _, err := filterString(t, stmt+";\n"); err != nil {
			t.Errorf("%q: err = %v", stmt, err)
		}
	}
}
