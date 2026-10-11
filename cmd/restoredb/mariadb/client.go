package mariadb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/xtrace"
)

// conn runs SQL against the server.
type conn interface {
	// query runs one statement and returns its rows.
	query(ctx context.Context, sql string) ([][]string, error)
	// execute runs statements, stopping at the first that fails.
	execute(ctx context.Context, sql string) error
	// load feeds a dump to the client with database as its default database,
	// in binary mode, so that client commands such as \u and connect, which
	// could switch databases, are sent to the server as text instead of run.
	// write is called with the client's stdin and must return once it has
	// written everything it will.
	load(ctx context.Context, database string, write func(io.Writer) error) error
}

// client is a conn that runs the mariadb command-line client, the same one the
// restore has always replayed dumps with.
type client struct {
	host, port, user, password string
	extra                      []string
	stdout, stderr             io.Writer
}

func (c *client) args(more ...string) []string {
	return slices.Concat([]string{
		"--host=" + c.host,
		"--port=" + c.port,
		"--user=" + c.user,
	}, more, c.extra)
}

func (c *client) command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "mariadb", args...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+c.password)
	return cmd
}

func (c *client) run(ctx context.Context, sql string, trace bool) (string, error) {
	cmd := c.command(ctx, c.args("--batch", "--skip-column-names", "--execute="+sql))
	if trace {
		xtrace.XTrace(c.stderr, cmd.Args)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if msg := clientError(errOut.String()); msg != "" {
			return "", fmt.Errorf("%s: %w", msg, err)
		}
		return "", err
	}
	return out.String(), nil
}

func (c *client) query(ctx context.Context, sql string) ([][]string, error) {
	out, err := c.run(ctx, sql, false)
	if err != nil {
		return nil, err
	}
	return parseBatch(out), nil
}

func (c *client) execute(ctx context.Context, sql string) error {
	_, err := c.run(ctx, sql, true)
	return err
}

func (c *client) load(ctx context.Context, database string, write func(io.Writer) error) error {
	cmd := c.command(ctx, c.args("--binary-mode", database))
	xtrace.XTrace(c.stderr, cmd.Args)
	cmd.Stdout, cmd.Stderr = c.stdout, c.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	writeErr := write(stdin)
	_ = stdin.Close()
	waitErr := cmd.Wait()
	if waitErr != nil && (writeErr == nil || errors.Is(writeErr, syscall.EPIPE)) {
		return waitErr
	}
	return errors.Join(writeErr, waitErr)
}

// clientError picks the client's ERROR lines out of its stderr, leaving out
// warnings, or returns all of it when it has none.
func clientError(stderr string) string {
	var errs []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(line, "ERROR") {
			errs = append(errs, line)
		}
	}
	if len(errs) == 0 {
		return strings.TrimSpace(stderr)
	}
	return strings.Join(errs, "; ")
}

// parseBatch splits the client's --batch output into rows of fields, undoing
// the escapes it writes for backslash, tab, newline and NUL.
func parseBatch(out string) [][]string {
	var rows [][]string
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		for i, f := range fields {
			fields[i] = unescapeBatch(f)
		}
		rows = append(rows, fields)
	}
	return rows
}

var batchEscapes = strings.NewReplacer(`\\`, `\`, `\t`, "\t", `\n`, "\n", `\0`, "\x00")

func unescapeBatch(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return batchEscapes.Replace(s)
}
