package cli_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
)

const argsNS = "apps"

func TestWrongArgumentCountsNameTheUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"version", "x"}, want: "version takes no arguments, got 1: b2bk version"},
		{args: []string{"parity", "a.yaml"}, want: "parity takes exactly two arguments, got 1: b2bk parity <generated.yaml>"},
		{args: []string{"validate"}, want: "validate takes exactly one argument, got none: b2bk validate <file.yaml>"},
		{args: []string{"migrate", "a", "b"}, want: "migrate takes exactly one argument, got 2: b2bk migrate"},
		{args: []string{"get", "sb", "repo"}, want: "get takes at most one argument, got 2: b2bk get [repositories|backups]"},
		{args: []string{cmdRestore, "a", "b"}, want: "restore takes exactly one name, got 2: b2bk restore"},
		{args: []string{cmdExec, "a", binRestic}, want: "exec needs -- before the command: b2bk exec"},
		{args: []string{cmdExec, "a", "b", "--", binRestic}, want: "exec takes exactly one name before --, got 2: b2bk exec"},
		{args: []string{cmdExec, "a", "--"}, want: "exec needs a command after --: b2bk exec"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			f := cli.NewTestFactoryIn(argsNS, nil, io.Discard, io.Discard, nil)
			err := cli.Execute(f, tt.args...)
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if !errors.Is(err, cli.ErrUsage) || !strings.HasPrefix(err.Error(), tt.args[0]+" ") {
				t.Errorf("error %q is not a usage error that starts with the command", err)
			}
			if strings.Contains(err.Error(), "arg(s)") {
				t.Errorf("error %q is cobra's", err)
			}
		})
	}
}
