package mariadb

import "strings"

type nameToken int

const (
	// nameOther is any token but a name: a number, a string, a variable, or
	// punctuation.
	nameOther nameToken = iota
	// nameWord is an unquoted word that can name something.
	nameWord
	// nameQuoted is a backquoted name.
	nameQuoted
	// nameDot is a dot after a name.
	nameDot
	// nameNumber is a word of digits so far. MariaDB reads one that goes on
	// with a letter, such as 1db, as a name.
	nameNumber
)

// qualifiedNames watches the whole text of a statement, as the client sends
// it, for a name qualified with a database, such as `other`.`t` or other.f(),
// and for the word SELECT. The statement is seen a byte at a time with the
// lexer's quote state, so a name in the part of a long statement passed on
// before the end is seen too.
type qualifiedNames struct {
	last nameToken
	// word holds the start of the current unquoted word, or of the last
	// name.
	word      []byte
	inWord    bool
	at        bool
	prevName  string
	qualifier string
	sawSelect bool
}

const maxNameWord = 64

func (q *qualifiedNames) reset() {
	*q = qualifiedNames{word: q.word[:0]}
}

func isNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' || c >= 0x80
}

// text sees the statement bytes p, whose quote state at emission is inString.
func (q *qualifiedNames) text(p []byte, inString byte) {
	for _, c := range p {
		q.byte(c, inString)
	}
}

func (q *qualifiedNames) byte(c, inString byte) {
	switch inString {
	case '\'', '"':
		q.endWord()
		q.last = nameOther
		return
	case '`':
		if q.last != nameQuoted {
			q.startName(nameQuoted)
			if c == '`' {
				return
			}
		}
		if len(q.word) < maxNameWord {
			q.word = append(q.word, c)
		}
		return
	}
	if q.inWord {
		if isNameStart(c) || isDigit(c) {
			if q.last == nameNumber && isNameStart(c) {
				q.last = nameWord
			}
			if len(q.word) < maxNameWord {
				q.word = append(q.word, c)
			}
			return
		}
		q.endWord()
	}
	switch {
	case c == '`':
	case isNameStart(c):
		if q.at {
			q.at = false
			q.inWord, q.last = true, nameOther
			q.word = q.word[:0]
			return
		}
		q.startName(nameWord)
		q.word = append(q.word, c)
		q.inWord = true
	case isDigit(c) && (q.at || q.last != nameDot):
		q.last = nameNumber
		if q.at {
			q.last = nameOther
		}
		q.at = false
		q.word = append(q.word[:0], c)
		q.inWord = true
	case isDigit(c):
		q.startName(nameWord)
		q.word = append(q.word, c)
		q.inWord = true
	case isClientSpace(c):
	case c == '.' && (q.last == nameWord || q.last == nameQuoted):
		q.prevName = string(q.word)
		q.last = nameDot
	case c == '@':
		q.at = true
		q.last = nameOther
	default:
		q.at = false
		q.last = nameOther
	}
}

// startName begins a name token; one right after a dot after a name makes the
// name before the dot a qualifier.
func (q *qualifiedNames) startName(kind nameToken) {
	if q.last == nameDot && q.qualifier == "" {
		q.qualifier = q.prevName
	}
	q.last = kind
	q.word = q.word[:0]
}

func (q *qualifiedNames) endWord() {
	if !q.inWord {
		return
	}
	q.inWord = false
	if q.last == nameWord && strings.EqualFold(string(q.word), "SELECT") {
		q.sawSelect = true
	}
}

// createsTable reports whether a statement is a CREATE TABLE or CREATE
// SEQUENCE.
func createsTable(stmt string) bool {
	toks := tokenize(skipLeadingComments(strings.TrimLeft(stmt, " \t\r\n")))
	if len(toks) == 0 || toks[0].kind != tokWord || !strings.EqualFold(toks[0].text, verbCreate) {
		return false
	}
	kind, _ := objectKind(toks[1:])
	return kind == kindTable || kind == kindSequence
}
