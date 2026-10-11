package mariadb

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// StagingSuffix names the schema a dump is restored into before the swap.
	StagingSuffix = "__b2bk_restore"
	// OldSuffix names the schema the replaced tables are moved into.
	OldSuffix = "__b2bk_old"

	maxIdentifier = 64
)

var (
	// ErrEmptyDump is returned when stdin holds no dump at all.
	ErrEmptyDump = errors.New("no dump on stdin")
	// ErrIncomplete is returned when the dump does not end with the dump tool's trailer.
	ErrIncomplete = errors.New(`the dump does not end with mariadb-dump's "-- Dump completed" trailer, ` +
		"so it was cut short")
	// ErrMultipleDatabases is returned for a dump that holds more than one database.
	ErrMultipleDatabases = errors.New("the dump holds more than one database; restoredb restores exactly one")
	// ErrUnsupportedObjects is returned when a schema or the dump holds objects the
	// swap cannot move atomically.
	ErrUnsupportedObjects = errors.New("views, triggers, stored routines and events cannot be swapped atomically")
	// ErrUnsupportedStatement is returned for a database-level statement the
	// restore does not know how to keep out of the live database.
	ErrUnsupportedStatement = errors.New("the dump changes a database itself")
	// ErrOldExists is returned when the rollback schema of an earlier restore is still there.
	ErrOldExists = errors.New("the rollback database of an earlier restore still holds tables")
	// ErrNoDatabase is returned when the target database does not exist or is not visible.
	ErrNoDatabase = errors.New("the database does not exist, or the user cannot see it")
	// ErrPrivileges is returned when the user cannot create and drop the staging and rollback schemas.
	ErrPrivileges = errors.New("the user cannot create, fill and drop the staging and rollback databases")
	// ErrNameTooLong is returned when a derived schema name exceeds MariaDB's identifier limit.
	ErrNameTooLong = errors.New("the database name is too long for the staging and rollback names")
)

// Names returns the staging and rollback schema names for database.
func Names(database string) (staging, old string, err error) {
	if database == "" {
		return "", "", fmt.Errorf("%w: no database given", ErrNoDatabase)
	}
	staging, old = database+StagingSuffix, database+OldSuffix
	if utf8.RuneCountInString(staging) > maxIdentifier {
		return "", "", fmt.Errorf("%w: %s is longer than %d characters", ErrNameTooLong, staging, maxIdentifier)
	}
	return staging, old, nil
}

// QuoteIdent quotes an identifier with backticks.
func QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func qualified(schema, table string) string {
	return QuoteIdent(schema) + "." + QuoteIdent(table)
}

// literal renders s as a hex string literal. It compares byte for byte with
// information_schema's case-insensitive columns and means the same under any
// sql_mode.
func literal(s string) string {
	return "X'" + hex.EncodeToString([]byte(s)) + "'"
}

// RenameStatement returns one RENAME TABLE statement that moves every table in
// current out of live into out, and every table in incoming from in into live.
// MariaDB applies a multi-table RENAME TABLE atomically: if any part fails,
// none of it happens. It returns "" when there is nothing to move.
func RenameStatement(live, in, out string, current, incoming []string) string {
	names := slices.Concat(current, incoming)
	slices.Sort(names)
	names = slices.Compact(names)
	moves := make([]string, 0, len(current)+len(incoming))
	for _, name := range names {
		if slices.Contains(current, name) {
			moves = append(moves, qualified(live, name)+" TO "+qualified(out, name))
		}
		if slices.Contains(incoming, name) {
			moves = append(moves, qualified(in, name)+" TO "+qualified(live, name))
		}
	}
	if len(moves) == 0 {
		return ""
	}
	return "RENAME TABLE " + strings.Join(moves, ", ")
}

const trailer = "-- Dump completed"

// DumpComplete reports whether tail, the end of a dump, ends with the trailer
// mariadb-dump and mysqldump write only after a successful dump: "-- Dump
// completed", followed by " on <date>" unless --skip-dump-date was given. A dump
// cut short does not end with it, since every data row is a line of its own
// that starts with INSERT.
func DumpComplete(tail []byte) bool {
	lines := strings.Split(string(tail), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return false
	}
	last := strings.TrimRight(lines[len(lines)-1], "\r")
	return last == trailer || strings.HasPrefix(last, trailer+" on ")
}

type lineKind int

const (
	linePass lineKind = iota
	lineDatabase
	lineObject
	lineUnsupported
	lineRefused
)

// classify reads the start of a dump line. It returns lineDatabase and the
// database named for a USE, CREATE DATABASE or DROP DATABASE line, all of which
// a --databases or --all-databases dump writes and none of which may reach the
// server. lineRefused means the line starts an object the swap cannot move.
func classify(line string) (lineKind, string) {
	line = strings.TrimRight(line, "\r\n")
	switch {
	case hasPrefixFold(line, "DELIMITER"):
		return lineRefused, "triggers, stored routines or events"
	case strings.HasPrefix(line, "/*!50001 "):
		return lineRefused, "views"
	case hasPrefixFold(line, "CREATE TABLE "), hasPrefixFold(line, "DROP TABLE "),
		hasPrefixFold(line, "CREATE SEQUENCE "), hasPrefixFold(line, "INSERT "):
		return lineObject, ""
	}

	body := skipVersionComment(line)
	for _, stmt := range []string{"USE ", "CREATE DATABASE ", "CREATE SCHEMA ", "DROP DATABASE ", "DROP SCHEMA "} {
		if !hasPrefixFold(body, stmt) {
			continue
		}
		name, ok := databaseName(body[len(stmt):])
		if !ok {
			return lineUnsupported, line
		}
		return lineDatabase, name
	}
	for _, stmt := range []string{"ALTER DATABASE", "ALTER SCHEMA", "CREATE OR REPLACE DATABASE",
		"CREATE OR REPLACE SCHEMA"} {
		if hasPrefixFold(body, stmt) {
			return lineUnsupported, line
		}
	}
	return linePass, ""
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// skipVersionComment strips the opening of a versioned comment, such as
// "/*!40000 " or "/*M!100616 ", from the start of s.
func skipVersionComment(s string) string {
	for _, open := range []string{"/*!", "/*M!"} {
		if rest, ok := strings.CutPrefix(s, open); ok {
			return strings.TrimLeft(strings.TrimLeft(rest, "0123456789"), " ")
		}
	}
	return s
}

// databaseName reads the backquoted name that follows an optional IF [NOT]
// EXISTS, itself perhaps in a versioned comment.
func databaseName(rest string) (string, bool) {
	for {
		rest = strings.TrimLeft(rest, " ")
		switch {
		case strings.HasPrefix(rest, "/*!"), strings.HasPrefix(rest, "/*M!"):
			rest = skipVersionComment(rest)
		case hasPrefixFold(rest, "IF NOT EXISTS"):
			rest = rest[len("IF NOT EXISTS"):]
		case hasPrefixFold(rest, "IF EXISTS"):
			rest = rest[len("IF EXISTS"):]
		case strings.HasPrefix(rest, "*/"):
			rest = rest[2:]
		default:
			return unquoteIdent(rest)
		}
	}
}

func unquoteIdent(s string) (string, bool) {
	if !strings.HasPrefix(s, "`") {
		return "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '`' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '`' {
			b.WriteByte('`')
			i++
			continue
		}
		return b.String(), b.Len() > 0
	}
	return "", false
}
