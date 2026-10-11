package cli_test

import (
	"context"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
)

const (
	excludeSELinux = "--exclude-xattr=security.selinux"
	cmdRestore     = "restore"
)

func TestRestoreLeavesTheSELinuxLabelAlone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sub     string
		include []string
		del     bool
		dryRun  bool
	}{
		{name: "a whole source"},
		{name: "a whole source with --delete", del: true},
		{name: "a dry run", del: true, dryRun: true},
		{name: "a path", sub: "/app/db"},
		{name: "an include", include: []string{"*.db"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args, err := cli.RestoreArgs("/data", tt.sub, "/data", tt.include, tt.del, tt.dryRun)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(args, excludeSELinux) {
				t.Errorf("restore args %q lack %s: restic would remove the label SELinux gives every new file, and SELinux refuses", args, excludeSELinux)
			}
		})
	}
}

func TestRestoreToNewPVCLeavesTheSELinuxLabelAlone(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var restores [][]string
	f := cli.NewTestFactory(pvcClient(t, pvcFixtures()...), io.Discard, func(runner.Options) (cli.RestoreExec, error) {
		inner := fakeRestic(false)
		return func(ctx context.Context, argv []string, stdout io.Writer) error {
			if len(argv) > 1 && argv[1] == cmdRestore {
				mu.Lock()
				restores = append(restores, argv)
				mu.Unlock()
			}
			return inner(ctx, argv, stdout)
		}, nil
	})
	if err := cli.RestoreToNewPVC(context.Background(), f, pvcScheduledBackup(), pvcNew); err != nil {
		t.Fatal(err)
	}
	if len(restores) == 0 {
		t.Fatal("no restic restore ran")
	}
	for _, argv := range restores {
		if !slices.Contains(argv, excludeSELinux) {
			t.Errorf("restore %q lacks %s", argv, excludeSELinux)
		}
	}
}
