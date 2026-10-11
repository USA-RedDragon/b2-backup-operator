package mariadb

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

const (
	tailSize = 4 << 10
	// headSize is how much of a statement is read before it is classified.
	headSize = 4 << 10
	// holdSize is how much of a statement, comments included, is held back
	// while it is classified.
	holdSize = 64 << 10
)

type stmtMode int

const (
	modeHold stmtMode = iota
	modePass
	modeSkip
)

// dumpFilter copies a dump a statement at a time, split the way the mariadb
// client splits it. It leaves out the statements that name a database, so
// every statement runs in the client's own database, and stops at the first
// statement the swap cannot handle: one that writes to a table in another
// database, changes the server, or starts an object RENAME TABLE cannot move.
// Each statement is held back until it is classified, so a refused statement
// never reaches the client. It keeps the end of the dump for DumpComplete.
type dumpFilter struct {
	// database is the database the dump's USE and CREATE DATABASE lines name.
	database  string
	tail      []byte
	lex       clientLexer
	active    bool
	mode      stmtMode
	head      []byte
	held      []byte
	out       []byte
	sawObject bool
}

func (f *dumpFilter) keepTail(p []byte) {
	f.tail = append(f.tail, p...)
	if over := len(f.tail) - tailSize; over > 0 {
		f.tail = append(f.tail[:0], f.tail[over:]...)
	}
}

// complete reports whether the dump ended between statements with the dump
// tool's trailer.
func (f *dumpFilter) complete() bool {
	return f.lex.idle() && DumpComplete(f.tail)
}

func (f *dumpFilter) copy(dst io.Writer, src *bufio.Reader) error {
	var carry []byte
	for {
		chunk, err := src.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		f.keepTail(chunk)
		if carry != nil {
			chunk = append(carry, chunk...)
			carry = nil
		}
		f.out = f.out[:0]
		n, ferr := f.lex.scan(chunk, !full, f.span)
		if ferr != nil {
			return ferr
		}
		if n < len(chunk) {
			carry = append([]byte(nil), chunk[n:]...)
		}
		if len(f.out) > 0 {
			if _, werr := dst.Write(f.out); werr != nil {
				return werr
			}
		}
		switch {
		case err == nil, full:
		case errors.Is(err, io.EOF):
			if f.active && f.mode == modeHold {
				f.out = f.out[:0]
				if err := f.decide(true); err != nil {
					return err
				}
				_, werr := dst.Write(f.out)
				return werr
			}
			return nil
		default:
			return err
		}
	}
}

// span routes bytes of one kind according to the statement they are part of.
func (f *dumpFilter) span(p []byte, kind byteKind) error {
	if kind == byteText && !f.active {
		f.active, f.mode = true, modeHold
		f.head, f.held = f.head[:0], f.held[:0]
	}
	if !f.active {
		f.out = append(f.out, p...)
		return nil
	}
	switch kind {
	case byteText:
		f.head = append(f.head, p[:min(len(p), max(0, headSize-len(f.head)))]...)
	case byteGap:
		if len(f.head) < headSize {
			f.head = append(f.head, ' ')
		}
	}
	switch f.mode {
	case modeHold:
		f.held = append(f.held, p...)
	case modePass:
		f.out = append(f.out, p...)
	}
	switch {
	case kind == byteEnd:
		f.active = false
		if f.mode == modeHold {
			return f.decide(true)
		}
	case f.mode == modeHold && (len(f.head) >= headSize || len(f.held) >= holdSize):
		return f.decide(false)
	}
	return nil
}

// decide classifies the statement held so far and passes it on, leaves it
// out, or refuses it.
func (f *dumpFilter) decide(complete bool) error {
	held := f.held
	f.held = f.held[:0]
	kind, arg := classify(string(f.head), complete)
	switch kind {
	case stmtRefused:
		return fmt.Errorf("%w: the dump has %s", ErrUnsupportedObjects, arg)
	case stmtUnsupported:
		return fmt.Errorf("%w: %.200s", ErrUnsupportedStatement, arg)
	case stmtServer:
		return fmt.Errorf("%w: %.200s", ErrServerStatement, arg)
	case stmtOtherDatabase:
		return fmt.Errorf("%w: %s", ErrOtherDatabase, arg)
	case stmtDatabase:
		if !complete {
			return fmt.Errorf("%w: %.200s", ErrUnsupportedStatement, f.head)
		}
		if err := f.sawDatabase(arg); err != nil {
			return err
		}
		f.mode = modeSkip
		return nil
	case stmtObject:
		f.sawObject = true
	}
	f.out = append(f.out, held...)
	f.mode = modePass
	return nil
}

func (f *dumpFilter) sawDatabase(name string) error {
	switch {
	case f.database == "" && f.sawObject:
		return fmt.Errorf("%w: it names %s only after its first table", ErrMultipleDatabases, QuoteIdent(name))
	case f.database == "":
		f.database = name
	case f.database != name:
		return fmt.Errorf("%w: it names %s and %s", ErrMultipleDatabases, QuoteIdent(f.database), QuoteIdent(name))
	}
	return nil
}
