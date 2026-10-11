package mariadb

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

const tailSize = 4 << 10

// dumpFilter copies a dump a line at a time. It leaves out the lines that name a
// database, so every statement runs in the client's own database, and stops at
// the first line the swap cannot handle. It keeps the end of the dump for
// DumpComplete.
type dumpFilter struct {
	// database is the database the dump's USE and CREATE DATABASE lines name.
	database string
	tail     []byte
}

func (f *dumpFilter) keepTail(p []byte) {
	f.tail = append(f.tail, p...)
	if over := len(f.tail) - tailSize; over > 0 {
		f.tail = append(f.tail[:0], f.tail[over:]...)
	}
}

func (f *dumpFilter) copy(dst io.Writer, src *bufio.Reader) error {
	atStart, sawObject := true, false
	for {
		chunk, err := src.ReadSlice('\n')
		if len(chunk) > 0 {
			f.keepTail(chunk)
			whole := chunk[len(chunk)-1] == '\n' || errors.Is(err, io.EOF)
			skip := false
			if atStart {
				kind, arg := classify(string(chunk))
				switch {
				case kind == lineRefused:
					return fmt.Errorf("%w: the dump has %s", ErrUnsupportedObjects, arg)
				case kind == lineUnsupported, kind == lineDatabase && !whole:
					return fmt.Errorf("%w: %.200s", ErrUnsupportedStatement, chunk)
				case kind == lineDatabase:
					if err := f.sawDatabase(arg, sawObject); err != nil {
						return err
					}
					skip = true
				case kind == lineObject:
					sawObject = true
				}
			}
			if !skip {
				if _, werr := dst.Write(chunk); werr != nil {
					return werr
				}
			}
			atStart = chunk[len(chunk)-1] == '\n'
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

func (f *dumpFilter) sawDatabase(name string, sawObject bool) error {
	switch {
	case f.database == "" && sawObject:
		return fmt.Errorf("%w: it names %s only after its first table", ErrMultipleDatabases, QuoteIdent(name))
	case f.database == "":
		f.database = name
	case f.database != name:
		return fmt.Errorf("%w: it names %s and %s", ErrMultipleDatabases, QuoteIdent(f.database), QuoteIdent(name))
	}
	return nil
}
