package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	cmdSnapshots = "snapshots"
	cmdExec      = "exec"
	binRestic    = "restic"
)

var errRunViaLogs = errors.New("ran through pod logs")

func TestJSONOutputIsOnlyRestics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		args        []string
		wantGroupBy bool
	}{
		{name: "snapshots --json", args: []string{cmdSnapshots, "sb/" + pvcBackup, "--json"}, wantGroupBy: true},
		{name: "exec passing --json through",
			args: []string{cmdExec, "sb/" + pvcBackup, "--", binRestic, cmdSnapshots, "--json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			var ran []string
			f := cli.NewTestFactoryIn(pvcNS, pvcClient(t, pvcFixtures()...), &out, &errOut,
				func(runner.Options) (cli.RestoreExec, error) {
					return func(_ context.Context, argv []string, stdout io.Writer) error {
						ran = argv
						_, err := io.WriteString(stdout, `[{"id":"`+pvcSnapshot+`"}]`+"\n")
						return err
					}, nil
				})
			err := cli.Execute(f, tt.args...)
			if err != nil {
				t.Fatalf("%v (stderr %q)", errors.Join(err, errRunViaLogs), errOut.String())
			}
			var parsed []map[string]any
			if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
				t.Errorf("stdout %q is not JSON: %v", out.String(), err)
			}
			if got := slices.Contains(ran, "--group-by="); got != tt.wantGroupBy {
				t.Errorf("--group-by= in %q = %t, want %t", ran, got, tt.wantGroupBy)
			}
		})
	}
}
