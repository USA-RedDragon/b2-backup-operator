package mariadb

import (
	"fmt"
	"slices"
	"strings"
)

// sqlModes follows the user variables that hold the sql_mode a dump saved at
// its start, such as mariadb-dump's
//
//	SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO'
//
// sql_mode decides how quotes read, so a SET may change it only to a literal
// the filter can check, to DEFAULT, or back to a variable saved that way and
// not changed since. Any other statement that uses a user variable could have
// changed one, so it forgets them all.
type sqlModes struct {
	saved map[string]bool
}

func (m *sqlModes) forget() {
	clear(m.saved)
}

// set follows the assignments of a SET statement, toks without the SET.
func (m *sqlModes) set(toks []token, stmt string) error {
	if m.saved == nil {
		m.saved = map[string]bool{}
	}
	for _, item := range splitTopLevel(toks) {
		for len(item) > 0 && item[0].kind == tokWord &&
			(strings.EqualFold(item[0].text, "SESSION") || strings.EqualFold(item[0].text, "LOCAL")) {
			item = item[1:]
		}
		eq := slices.IndexFunc(item, func(t token) bool { return t.kind == tokPunct && t.text == "=" })
		if eq < 0 {
			continue
		}
		lhs, rhs := item[:eq], item[eq+1:]
		if n := len(lhs); n > 0 && lhs[n-1].kind == tokPunct && lhs[n-1].text == ":" {
			lhs = lhs[:n-1]
		}
		switch {
		case isSQLMode(lhs):
			if !m.knownMode(rhs) {
				return fmt.Errorf("%w: an sql_mode set to something other than a literal the restore can read "+
					"or the mode the dump saved at its start: %.200s", ErrUnsupportedStatement, stmt)
			}
			continue
		case userVar(lhs) != "":
			name := userVar(lhs)
			m.taint(rhs)
			if len(rhs) > 2 && rhs[0].text == "@" && rhs[1].text == "@" && isSQLMode(rhs) {
				m.saved[name] = true
			} else {
				delete(m.saved, name)
			}
			continue
		}
		m.taint(rhs)
	}
	return nil
}

// knownMode reports whether rhs sets a mode the filter can follow.
func (m *sqlModes) knownMode(rhs []token) bool {
	switch {
	case len(rhs) == 1 && rhs[0].kind == tokString:
		for mode := range strings.SplitSeq(strings.ToUpper(rhs[0].text), ",") {
			if slices.Contains(lexingModes, strings.TrimSpace(mode)) {
				return false
			}
		}
		return true
	case len(rhs) == 1 && rhs[0].kind == tokWord:
		return strings.EqualFold(rhs[0].text, "DEFAULT")
	}
	name := userVar(rhs)
	return name != "" && m.saved[name]
}

// taint forgets every user variable rhs names, since := can assign it.
func (m *sqlModes) taint(rhs []token) {
	for i := 0; i+1 < len(rhs); i++ {
		if rhs[i].text != "@" || rhs[i].kind != tokPunct {
			continue
		}
		if rhs[i+1].kind == tokPunct && rhs[i+1].text == "@" {
			i++
			continue
		}
		delete(m.saved, strings.ToLower(rhs[i+1].text))
	}
}

// userVar returns the name of the user variable toks is, such as @x, or "".
func userVar(toks []token) string {
	if len(toks) != 2 || toks[0].kind != tokPunct || toks[0].text != "@" || toks[1].kind == tokPunct {
		return ""
	}
	return strings.ToLower(toks[1].text)
}

// isSQLMode reports whether toks name the session's sql_mode, as sql_mode,
// @@sql_mode or @@SESSION.sql_mode.
func isSQLMode(toks []token) bool {
	var words []string
	for _, t := range toks {
		switch {
		case t.kind == tokPunct && (t.text == "@" || t.text == "."):
		case t.kind == tokWord && (strings.EqualFold(t.text, "SESSION") || strings.EqualFold(t.text, "LOCAL")):
		default:
			words = append(words, strings.ToUpper(t.text))
		}
	}
	return len(words) == 1 && words[0] == "SQL_MODE"
}

// splitTopLevel splits toks at the commas outside parentheses.
func splitTopLevel(toks []token) [][]token {
	var items [][]token
	depth, start := 0, 0
	for i, t := range toks {
		if t.kind != tokPunct {
			continue
		}
		switch t.text {
		case "(":
			depth++
		case ")":
			depth--
		case ",":
			if depth == 0 {
				items = append(items, toks[start:i])
				start = i + 1
			}
		}
	}
	return append(items, toks[start:])
}
