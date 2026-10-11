package dumpdb_test

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/USA-RedDragon/b2-backup-operator/internal/dbtools/dumpdb"
	"github.com/spf13/cobra"
)

func testCmd(out *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd
}

func TestFailedCommandKeepsItsOwnOutputAndStatus(t *testing.T) {
	var out bytes.Buffer
	cmd := testCmd(&out)

	err := dumpdb.RunCmd(cmd, "sh", []string{"-c", "echo the real reason >&2; exit 42"}, nil)

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected an exit status, got %v", err)
	}
	if got := exitErr.ExitCode(); got != 42 {
		t.Errorf("exit code = %d, want 42", got)
	}
	if !strings.Contains(out.String(), "the real reason") {
		t.Errorf("the command's own message was lost:\n%s", out.String())
	}
	if !cmd.SilenceUsage {
		t.Error("usage would be printed over the command's own message")
	}
}

// Failing to start the command is our error, not the command's.
func TestUnstartableCommandIsAnOrdinaryError(t *testing.T) {
	var out bytes.Buffer

	err := dumpdb.RunCmd(testCmd(&out), "definitely-not-a-real-binary", nil, nil)
	if err == nil {
		t.Fatal("expected an error when the command cannot be started")
	}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		t.Errorf("a missing binary should not look like an exit status: %v", err)
	}
}

func TestDryRunDoesNotExecute(t *testing.T) {
	var out bytes.Buffer
	// `false` would exit 1 if it ran.
	if err := dumpdb.RunCmd(testCmd(&out), "false", nil, &dumpdb.RunOpts{DryRun: true}); err != nil {
		t.Errorf("dry run should not execute: %v", err)
	}
}

// RunCmd silences usage, so a bad flag -- rejected before it runs -- still gets it.
func TestBadFlagStillPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	root := &cobra.Command{Use: "root"}
	sub := &cobra.Command{
		Use: "sub",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return dumpdb.RunCmd(cmd, "true", nil, nil)
		},
	}
	root.AddCommand(sub)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"sub", "--bogus"})

	if err := root.Execute(); err == nil {
		t.Fatal("expected the flag to be rejected")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("a bad flag should still print usage:\n%s", out.String())
	}
}
