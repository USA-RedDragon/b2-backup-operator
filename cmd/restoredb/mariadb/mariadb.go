package mariadb

import (
	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/cobrax"
	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/dumpdb"
	"github.com/spf13/cobra"
)

func New() *cobra.Command {
	var mount, host, port, database, username, password string
	var dryRun bool

	fs := &cobrax.Flags{}
	cmd := &cobra.Command{
		Use:   "mariadb",
		Short: "Restore a MariaDB database",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := fs.Resolve(); err != nil {
				return err
			}

			// `dumpdb mariadb` writes plain SQL including --add-drop-table, so the
			// client replays it as-is.
			args = append([]string{
				"--host=" + host,
				"--port=" + port,
				"--user=" + username,
				database,
			}, args...)

			return dumpdb.RunCmd(cmd, "mariadb", args, &dumpdb.RunOpts{
				Envs:   []string{"MYSQL_PWD=" + password},
				DryRun: dryRun,
			})
		},
	}

	fs.FlagSet = cmd.Flags()
	fs.String(&mount, "secret-mount", "", "/mariadb", "Directory where secrets are mounted")
	fs.String(&host, dumpdb.FlagHost, "H", "", "Database host",
		cobrax.Env("DB_HOST"))
	fs.String(&port, dumpdb.FlagPort, "P", "3306", "Database port",
		cobrax.Env("DB_PORT"))
	fs.String(&database, dumpdb.FlagDatabase, "d", "", "Database name",
		cobrax.Env("DB_DATABASE"))
	fs.String(&username, dumpdb.FlagUsername, "u", "", "Database user",
		cobrax.Env("DB_USERNAME"))
	fs.String(&password, dumpdb.FlagPassword, "p", "", "Database password",
		cobrax.Env("DB_PASSWORD"), cobrax.SecretFile(&mount, "mariadb-password"))
	fs.Bool(&dryRun, dumpdb.FlagDryRun, "", false, "Dry run",
		cobrax.Env("DB_DRY_RUN"))

	return cmd
}
