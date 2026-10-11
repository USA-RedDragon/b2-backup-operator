package postgres

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// ErrUnsupportedDump is returned for a plain dump the restore cannot follow.
var ErrUnsupportedDump = errors.New("the plain dump has something restoredb cannot restore in one transaction")

// PlainFilter passes a plain pg_dump script on to psql while following it the
// way psql reads it: statements, quoted text, and the data lines of each COPY
// ... FROM stdin up to its \. line.
//
// pg_dump wraps large-object data in its own BEGIN; and COMMIT; lines. Under
// psql --single-transaction the BEGIN is only a warning, but the COMMIT would
// commit everything before it, and the rest of the dump would then run outside
// any transaction. PlainFilter turns those lines into comments when they stand
// alone between statements, outside COPY data, so the whole dump stays in the
// one transaction psql commits at the end. Any other statement that ends,
// rolls back or prepares a transaction is refused.
type PlainFilter struct {
	lex       *lexer
	inCopy    bool
	lineStart bool
}

// NewPlainFilter returns a PlainFilter at the start of a dump.
func NewPlainFilter() *PlainFilter {
	return &PlainFilter{lex: newLexer(), lineStart: true}
}

// Copy copies src to dst as described on PlainFilter. It stops at the first
// statement it refuses, before passing on the line that would complete it.
func (f *PlainFilter) Copy(dst io.Writer, src io.Reader) error {
	in, ok := src.(*bufio.Reader)
	if !ok {
		in = bufio.NewReaderSize(src, 64<<10)
	}
	for {
		chunk, err := in.ReadSlice('\n')
		if len(chunk) > 0 {
			out, ferr := f.line(chunk, err == nil || errors.Is(err, io.EOF))
			if ferr != nil {
				return ferr
			}
			if _, werr := dst.Write(out); werr != nil {
				return werr
			}
		}
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}

// line handles chunk, a whole line when whole is set, or else part of one.
func (f *PlainFilter) line(chunk []byte, whole bool) ([]byte, error) {
	start := f.lineStart
	f.lineStart = chunk[len(chunk)-1] == '\n'
	if f.inCopy {
		if start && whole && isCopyEnd(chunk) {
			f.inCopy = false
		}
		return chunk, nil
	}
	if start && whole && f.lex.idle() {
		if bytes.Equal(trimEOL(chunk), []byte("BEGIN;")) || bytes.Equal(trimEOL(chunk), []byte("COMMIT;")) {
			return append([]byte("-- "), chunk...), nil
		}
	}
	if err := f.lex.feed(chunk); err != nil {
		return nil, fmt.Errorf("%w; at %.200q", err, chunk)
	}
	if f.lineStart && f.lex.copyPending {
		f.lex.copyPending = false
		f.inCopy = true
	}
	return chunk, nil
}

func trimEOL(line []byte) []byte {
	return bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
}

// isCopyEnd reports whether line ends COPY data the way psql sees it: \.
// alone, then a newline.
func isCopyEnd(line []byte) bool {
	return string(line) == "\\.\n" || string(line) == "\\.\r\n"
}
