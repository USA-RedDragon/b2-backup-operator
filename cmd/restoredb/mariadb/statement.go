package mariadb

import (
	"slices"
	"strconv"
	"strings"
)

type stmtKind int

const (
	stmtPass stmtKind = iota
	// stmtDatabase is a USE, CREATE DATABASE or DROP DATABASE, which a
	// --databases or --all-databases dump writes and which must not reach the
	// server. Its argument is the database named.
	stmtDatabase
	// stmtObject creates, drops or fills a table or sequence.
	stmtObject
	// stmtRefused starts an object the swap cannot move.
	stmtRefused
	// stmtUnsupported is a statement the restore cannot keep out of the live
	// database, or cannot check.
	stmtUnsupported
	// stmtServer changes the server, replication or accounts.
	stmtServer
	// stmtOtherDatabase writes to a table it names with a database.
	stmtOtherDatabase
)

const (
	verbCreate      = "CREATE"
	verbDrop        = "DROP"
	verbAlter       = "ALTER"
	kindSequence    = "SEQUENCE"
	objectsTriggers = "triggers, stored routines or events"
	objectsViews    = "views"
	tooLong         = "a statement too long to check: "
	// neverVersion is the version from which an executable comment is taken
	// as one no server runs, such as mariadb-dump's /*M!999999\- ... */.
	neverVersion = 900000
)

var (
	serverVerbs = []string{
		"BACKUP", "BINLOG", "CALL", "CHANGE", "DEALLOCATE", "EXECUTE", "FLUSH", "GRANT", "HANDLER", "INSTALL",
		"KILL", "LOAD", "PREPARE", "PURGE", "RESET", "REVOKE", "SHUTDOWN", "STOP", "UNINSTALL", "XA",
	}
	serverObjects  = []string{"USER", "ROLE", "SERVER", "TABLESPACE", "LOGFILE", "INSTANCE"}
	routineObjects = []string{"TRIGGER", "PROCEDURE", "FUNCTION", "EVENT", "PACKAGE"}
	tableObjects   = []string{"TABLE", "TABLES", kindSequence}
	// lexingModes change how the client and server read quotes and
	// backslashes, which the restore's lexer does not follow.
	lexingModes = []string{"ANSI", "ANSI_QUOTES", "NO_BACKSLASH_ESCAPES", "ORACLE", "POSTGRESQL", "DB2",
		"MAXDB", "MSSQL"}
)

// classify reads a statement as the client sends it, without its comments.
// complete is false when stmt is only the start of a longer statement.
func classify(stmt string, complete bool) (stmtKind, string) {
	s := strings.TrimLeft(stmt, " \t\r\n")
	switch {
	case hasPrefixFold(s, "DELIMITER"):
		return stmtRefused, objectsTriggers
	case strings.HasPrefix(s, "/*!50001 "):
		return stmtRefused, objectsViews
	}

	body := skipLeadingComments(s)
	for _, verb := range []string{"USE ", "CREATE DATABASE ", "CREATE SCHEMA ", "DROP DATABASE ", "DROP SCHEMA "} {
		if !hasPrefixFold(body, verb) {
			continue
		}
		name, ok := databaseName(body[len(verb):])
		if !ok {
			return stmtUnsupported, s
		}
		return stmtDatabase, name
	}
	for _, verb := range []string{"ALTER DATABASE", "ALTER SCHEMA", "CREATE OR REPLACE DATABASE",
		"CREATE OR REPLACE SCHEMA"} {
		if hasPrefixFold(body, verb) {
			return stmtUnsupported, s
		}
	}

	toks := tokenize(body)
	if len(toks) == 0 || toks[0].kind != tokWord {
		return stmtPass, ""
	}
	verb := strings.ToUpper(toks[0].text)
	switch verb {
	case "INSERT", "REPLACE", "UPDATE", "DELETE", "TRUNCATE":
		name, qualified, found := target(toks[1:], false)
		switch {
		case qualified:
			return stmtOtherDatabase, name
		case !found && !complete:
			return stmtUnsupported, tooLong + s
		}
		if verb == "INSERT" {
			return stmtObject, ""
		}
		return stmtPass, ""
	case verbCreate, verbAlter, verbDrop, "RENAME", "LOCK":
		return classifyDDL(verb, toks, s, complete)
	case "SELECT":
		return classifySelect(toks, s, complete)
	case "SET":
		return classifySet(toks, s, complete)
	case "START":
		if len(toks) > 1 && strings.EqualFold(toks[1].text, "TRANSACTION") {
			return stmtPass, ""
		}
		return stmtServer, s
	}
	if slices.Contains(serverVerbs, verb) {
		return stmtServer, s
	}
	return stmtPass, ""
}

func classifyDDL(verb string, toks []token, s string, complete bool) (stmtKind, string) {
	kind, at := objectKind(toks[1:])
	at++
	switch {
	case slices.Contains(serverObjects, kind):
		return stmtServer, s
	case kind == "VIEW" && verb == verbCreate:
		return stmtRefused, objectsViews
	case slices.Contains(routineObjects, kind) && verb == verbCreate:
		return stmtRefused, objectsTriggers
	case kind == "INDEX":
		if !complete {
			return stmtUnsupported, tooLong + s
		}
		for i := at + 1; i < len(toks); i++ {
			if toks[i].kind == tokWord && strings.EqualFold(toks[i].text, "ON") {
				if name, qualified, _ := target(toks[i+1:], false); qualified {
					return stmtOtherDatabase, name
				}
				break
			}
		}
		return stmtPass, ""
	case !slices.Contains(tableObjects, kind):
		return stmtPass, ""
	}
	list := verb == verbDrop || verb == "LOCK" || verb == "RENAME"
	if (list || verb == verbAlter) && !complete {
		return stmtUnsupported, tooLong + s
	}
	name, qualified, found := target(toks[at+1:], list)
	switch {
	case qualified:
		return stmtOtherDatabase, name
	case !found && !complete:
		return stmtUnsupported, tooLong + s
	}
	if verb == verbAlter {
		for i := at + 1; i < len(toks); i++ {
			if toks[i].kind == tokWord && strings.EqualFold(toks[i].text, "RENAME") {
				if name, qualified, _ := target(toks[i+1:], false); qualified {
					return stmtOtherDatabase, name
				}
			}
		}
	}
	if verb == verbCreate || verb == verbDrop && kind != kindSequence {
		return stmtObject, ""
	}
	return stmtPass, ""
}

// classifySelect refuses a SELECT that writes a file on the server, and a
// sequence function on a sequence named with a database.
func classifySelect(toks []token, s string, complete bool) (stmtKind, string) {
	if !complete {
		return stmtUnsupported, tooLong + s
	}
	for _, t := range toks {
		if t.kind == tokWord && (strings.EqualFold(t.text, "OUTFILE") || strings.EqualFold(t.text, "DUMPFILE")) {
			return stmtServer, s
		}
	}
	if len(toks) < 3 || toks[1].kind != tokWord || toks[2].text != "(" {
		return stmtPass, ""
	}
	switch strings.ToUpper(toks[1].text) {
	case "SETVAL", "NEXTVAL", "LASTVAL", "PREVIOUS":
		if name, qualified, _ := target(toks[3:], false); qualified {
			return stmtOtherDatabase, name
		}
	}
	return stmtPass, ""
}

// classifySet refuses a SET that reaches past the session, and one that turns
// on an sql_mode under which quotes and backslashes read differently.
func classifySet(toks []token, s string, complete bool) (stmtKind, string) {
	if !complete {
		return stmtUnsupported, tooLong + s
	}
	if len(toks) > 1 && toks[1].kind == tokWord {
		switch strings.ToUpper(toks[1].text) {
		case "PASSWORD", "ROLE", "DEFAULT", "STATEMENT":
			return stmtServer, s
		}
	}
	sqlMode := false
	for _, t := range toks {
		if t.kind == tokWord {
			switch strings.ToUpper(t.text) {
			case "GLOBAL", "PERSIST", "PERSIST_ONLY":
				return stmtServer, s
			case "SQL_MODE":
				sqlMode = true
			}
		}
	}
	if !sqlMode {
		return stmtPass, ""
	}
	for _, t := range toks {
		if t.kind != tokString && t.kind != tokWord {
			continue
		}
		for mode := range strings.SplitSeq(strings.ToUpper(t.text), ",") {
			if slices.Contains(lexingModes, strings.TrimSpace(mode)) {
				return stmtUnsupported, "an sql_mode the restore cannot read the dump under: " + s
			}
		}
	}
	return stmtPass, ""
}

// objectKind finds the kind of object a CREATE, ALTER, DROP, RENAME or LOCK
// names, such as TABLE or VIEW, and its index in toks.
func objectKind(toks []token) (string, int) {
	for i, t := range toks {
		switch {
		case t.kind == tokWord:
			word := strings.ToUpper(t.text)
			if slices.Contains(serverObjects, word) || slices.Contains(routineObjects, word) ||
				slices.Contains(tableObjects, word) || word == "VIEW" || word == "INDEX" {
				return word, i
			}
		case t.kind == tokPunct && (t.text == "(" || t.text == ";"):
			return "", i
		}
	}
	return "", len(toks)
}

// target reads the table name at the start of toks, after any keywords, and
// with list the names after each comma or TO that follows. It returns the
// first name that is qualified with a database. found reports whether a
// whole name was read: toks may be only the start of a statement.
func target(toks []token, list bool) (name string, qualified, found bool) {
	i := 0
	for i < len(toks) && toks[i].kind == tokWord && isKeyword(toks[i].text) {
		i++
	}
	for i < len(toks) {
		if name, ok := qualifiedAt(toks, i); ok {
			return name, true, true
		}
		found = found || i+1 < len(toks)
		if !list {
			return "", false, found
		}
		for i++; i < len(toks); i++ {
			t := toks[i]
			if t.kind == tokPunct && (t.text == ";" || t.text == "(") {
				return "", false, found
			}
			if t.kind == tokPunct && t.text == "," || t.kind == tokWord && strings.EqualFold(t.text, "TO") {
				i++
				break
			}
		}
	}
	return "", false, found
}

func qualifiedAt(toks []token, i int) (string, bool) {
	if i+2 >= len(toks) || toks[i+1].kind != tokPunct || toks[i+1].text != "." {
		return "", false
	}
	if toks[i].kind != tokIdent && (toks[i].kind != tokWord || isDigit(toks[i].text[0])) {
		return "", false
	}
	return QuoteIdent(toks[i].text) + "." + QuoteIdent(toks[i+2].text), true
}

// isKeyword reports whether word is one that may come between a statement's
// verb and the table it names.
func isKeyword(word string) bool {
	switch strings.ToUpper(word) {
	case "LOW_PRIORITY", "DELAYED", "HIGH_PRIORITY", "IGNORE", "INTO", "QUICK", "FROM", "TABLE", "TABLES",
		kindSequence, "IF", "NOT", "EXISTS", "TEMPORARY", "ONLINE", "OR", "REPLACE", "WAIT", "NOWAIT", "TO", "AS":
		return true
	}
	return false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

type tokenKind int

const (
	tokWord tokenKind = iota
	tokIdent
	tokString
	tokPunct
)

type token struct {
	kind tokenKind
	text string
}

// tokenize splits the start of a statement into words, backquoted
// identifiers, quoted strings and punctuation. The opening and closing of an
// executable comment are left out, since the server reads what is inside it.
func tokenize(s string) []token {
	var toks []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isClientSpace(c):
			i++
		case strings.HasPrefix(s[i:], "/*!") || strings.HasPrefix(s[i:], "/*M!"):
			rest := skipVersionComment(s[i:])
			i = len(s) - len(rest)
		case strings.HasPrefix(s[i:], "*/"):
			i += 2
		case c == '`':
			name, n := readQuoted(s[i:], '`')
			toks = append(toks, token{tokIdent, name})
			i += n
		case c == '\'' || c == '"':
			text, n := readQuoted(s[i:], c)
			toks = append(toks, token{tokString, text})
			i += n
		case isWordByte(c):
			j := i
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			toks = append(toks, token{tokWord, s[i:j]})
			i = j
		default:
			toks = append(toks, token{tokPunct, s[i : i+1]})
			i++
		}
	}
	return toks
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || isDigit(c) || c == '_' || c == '$' || c >= 0x80
}

// readQuoted reads the quoted text at the start of s, whose first byte is
// quote, and returns it unescaped and how many bytes it took.
func readQuoted(s string, quote byte) (string, int) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\' && quote != '`' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case s[i] == quote && i+1 < len(s) && s[i+1] == quote:
			b.WriteByte(quote)
			i++
		case s[i] == quote:
			return b.String(), i + 1
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), len(s)
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// skipLeadingComments strips the executable comment openings, such as
// "/*!40000 ", and closings at the start of s, and skips whole any comment
// for a version no server has.
func skipLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "*/"):
			s = s[2:]
		case strings.HasPrefix(s, "/*!") || strings.HasPrefix(s, "/*M!"):
			rest := skipVersionComment(s)
			if commentVersion(s) >= neverVersion {
				if _, after, ok := strings.Cut(rest, "*/"); ok {
					rest = after
				}
			}
			s = rest
		default:
			return s
		}
	}
}

func commentVersion(s string) int {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "/*!"), "/*M!")
	end := 0
	for end < len(s) && isDigit(s[end]) {
		end++
	}
	v, _ := strconv.Atoi(s[:end])
	return v
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
