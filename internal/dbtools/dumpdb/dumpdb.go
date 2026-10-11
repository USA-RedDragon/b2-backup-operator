package dumpdb

import (
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/xtrace"
	"github.com/spf13/cobra"
)

const (
	FlagHost     = "host"
	FlagPort     = "port"
	FlagDatabase = "database"
	FlagUsername = "username"
	FlagPassword = "password"
	FlagDryRun   = "dry-run"
)

type RunOpts struct {
	Envs   []string
	Redact []string
	DryRun bool
	Stdin  io.Reader
}

func RunCmd(cmd *cobra.Command, name string, args []string, opts *RunOpts) error {
	cmd.SilenceUsage = true

	if opts == nil {
		opts = &RunOpts{}
	}

	e := exec.CommandContext(cmd.Context(), name, args...)
	e.Env = append(os.Environ(), opts.Envs...)
	e.Stdin = cmd.InOrStdin()
	if opts.Stdin != nil {
		e.Stdin = opts.Stdin
	}
	e.Stdout = cmd.OutOrStdout()
	e.Stderr = cmd.ErrOrStderr()

	trace := xtrace.XTraceString(e.Args)
	for _, v := range opts.Redact {
		trace = strings.ReplaceAll(trace, v, "***")
	}
	_, _ = io.WriteString(e.Stderr, trace)

	if opts.DryRun {
		return nil
	}

	return e.Run()
}
