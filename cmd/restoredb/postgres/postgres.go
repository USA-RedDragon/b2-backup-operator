package postgres

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/cobrax"
	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/dumpdb"
	"github.com/spf13/cobra"
)

// ErrEmptyDump is returned when stdin holds no dump at all.
var ErrEmptyDump = errors.New("no dump on stdin")

var customMagic = []byte("PGDMP")

var plainTrailer = []byte("-- PostgreSQL database dump complete")

// PlainDumpComplete reports whether the end of a plain SQL dump holds the line
// pg_dump writes when it finishes. A dump cut short by a failed download lacks
// it.
func PlainDumpComplete(tail []byte) bool {
	return bytes.Contains(tail, plainTrailer)
}

// IsCustomFormat reports whether head starts with the pg_dump custom-format
// magic bytes.
func IsCustomFormat(head []byte) bool {
	return bytes.HasPrefix(head, customMagic)
}

func New() *cobra.Command {
	var mount, host, port, database, username, password string
	var dryRun bool

	fs := &cobrax.Flags{}
	cmd := &cobra.Command{
		Use:     "postgres",
		Aliases: []string{"cnpg"},
		Short:   "Restore a Postgres database",
		Long: `Restore a Postgres database from a dump read from stdin.

A pg_dump custom-format dump, as written by dumpdb postgres, is restored with
pg_restore. Anything else is taken as a plain SQL dump from pg_dump and run with
psql. Both run in a single transaction, so a failed restore leaves the database
untouched. pg_restore reads the whole archive before it commits; psql commits
when its input ends, so a plain dump that ends without pg_dump's "dump complete"
trailer stops psql first, and nothing is committed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := fs.Resolve(); err != nil {
				return err
			}

			in := bufio.NewReader(cmd.InOrStdin())
			head, err := in.Peek(len(customMagic))
			if err != nil && !errors.Is(err, io.EOF) {
				cmd.SilenceUsage = true
				return fmt.Errorf("read dump: %w", err)
			}
			if len(head) == 0 {
				cmd.SilenceUsage = true
				return ErrEmptyDump
			}

			name := "psql"
			opts := []string{"-v", "ON_ERROR_STOP=1", "--single-transaction", "--no-psqlrc", "--quiet"}
			complete := PlainDumpComplete
			if IsCustomFormat(head) {
				name = "pg_restore"
				opts = []string{"--clean", "--if-exists", "--no-owner", "--single-transaction"}
				complete = nil
			}
			conn := []string{
				"--host=" + host,
				"--port=" + port,
				"--username=" + username,
				"--dbname=" + database,
			}

			return dumpdb.RunCmd(cmd, name, slices.Concat(opts, conn, args), &dumpdb.RunOpts{
				Envs:     []string{"PGPASSWORD=" + password},
				DryRun:   dryRun,
				Stdin:    in,
				Complete: complete,
			})
		},
	}

	fs.FlagSet = cmd.Flags()
	fs.String(&mount, "secret-mount", "", "/postgresql-app", "Directory where secrets are mounted")
	fs.String(&host, dumpdb.FlagHost, "H", "", "Database host",
		cobrax.Env("DB_HOST"), cobrax.SecretFile(&mount, "host"))
	fs.String(&port, dumpdb.FlagPort, "P", "5432", "Database port",
		cobrax.Env("DB_PORT"), cobrax.SecretFile(&mount, "port"))
	fs.String(&database, dumpdb.FlagDatabase, "d", "", "Database name",
		cobrax.Env("DB_DATABASE"), cobrax.SecretFile(&mount, "dbname"))
	fs.String(&username, dumpdb.FlagUsername, "u", "", "Database user",
		cobrax.Env("DB_USERNAME"), cobrax.SecretFile(&mount, "username"))
	fs.String(&password, dumpdb.FlagPassword, "p", "", "Database password",
		cobrax.Env("DB_PASSWORD"), cobrax.SecretFile(&mount, "password"))
	fs.Bool(&dryRun, dumpdb.FlagDryRun, "", false, "Dry run",
		cobrax.Env("DB_DRY_RUN"))

	return cmd
}
