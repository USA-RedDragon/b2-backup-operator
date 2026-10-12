package cli_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: lab
  cluster: {server: "https://127.0.0.1:1"}
users:
- name: admin
contexts:
- name: lab-admin
  context: {cluster: lab, user: admin}
current-context: lab-admin
`

// completionFactory returns a Factory over a fake cluster and a kubeconfig
// that points nowhere, so no completion can reach a real cluster.
func completionFactory(t *testing.T, out io.Writer) *cli.Factory {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	objs := append(argsBackups(2), &corev1.Namespace{Name: argsNS})
	f := cli.NewTestFactoryIn(argsNS, argsClient(t, objs...), out, io.Discard, nil)
	*f.ConfigFlags.KubeConfig = path
	return f
}

// complete runs b2bk __complete args and returns its completions and directive.
func complete(t *testing.T, args ...string) ([]string, cobra.ShellCompDirective) {
	t.Helper()
	var out bytes.Buffer
	f := completionFactory(t, &out)
	if err := cli.Execute(f, append([]string{cobra.ShellCompRequestCmd}, args...)...); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	last := lines[len(lines)-1]
	n, err := strconv.Atoi(strings.TrimPrefix(last, ":"))
	if !strings.HasPrefix(last, ":") || err != nil {
		t.Fatalf("%v: no directive in %q", args, out.String())
	}
	return lines[:len(lines)-1], cobra.ShellCompDirective(n)
}

// pathCompletions are the only positions that complete local paths: files with
// any name, YAML manifests (and the directories leading to them), or directories.
var pathCompletions = map[string]cobra.ShellCompDirective{
	flagKubeconfig:            cobra.ShellCompDirectiveDefault,
	"--certificate-authority": cobra.ShellCompDirectiveDefault,
	"--client-certificate":    cobra.ShellCompDirectiveDefault,
	"--client-key":            cobra.ShellCompDirectiveDefault,
	"--cache-dir":             cobra.ShellCompDirectiveFilterDirs,
	"restore --to":            cobra.ShellCompDirectiveFilterDirs,
	"validate #0":             cobra.ShellCompDirectiveFilterFileExt,
	"parity #0":               cobra.ShellCompDirectiveFilterFileExt,
	"parity #1":               cobra.ShellCompDirectiveFilterFileExt,
	"parity --from-secret":    cobra.ShellCompDirectiveFilterFileExt,
	"migrate #0":              cobra.ShellCompDirectiveFilterFileExt,
	"migrate --from-secret":   cobra.ShellCompDirectiveFilterFileExt,
}

const (
	flagKubeconfig = "--kubeconfig"
	compNoFiles    = ":4\n"
	compManifests  = "yaml\nyml\n:8\n"
)

var connectionPaths = map[string]bool{
	flagKubeconfig: true, "--certificate-authority": true, "--client-certificate": true,
	"--client-key": true, "--cache-dir": true,
}

type completionCase struct {
	key  string
	args []string
}

// completionCases lists every positional argument, up to three deep, and
// every flag value of cmd and its subcommands.
func completionCases(cmd *cobra.Command) []completionCase {
	path := strings.Fields(cmd.CommandPath())[1:]
	name := strings.Join(path, " ")
	var cases []completionCase
	for i := range 3 {
		args := append(append([]string{}, path...), slices.Repeat([]string{"x"}, i)...)
		cases = append(cases, completionCase{strings.TrimSpace(name + " #" + strconv.Itoa(i)), append(args, "")})
	}
	if strings.Contains(cmd.Use, " -- ") {
		cases = append(cases,
			completionCase{name + " after --", append(append([]string{}, path...), "--", "")},
			completionCase{name + " command", append(append([]string{}, path...), "x", "--", "restic", "")})
	}
	seen := map[string]bool{}
	visit := func(fl *pflag.Flag) {
		if seen[fl.Name] || fl.Hidden || fl.NoOptDefVal != "" {
			return
		}
		seen[fl.Name] = true
		key := strings.TrimSpace(name + " --" + fl.Name)
		if connectionPaths["--"+fl.Name] {
			key = "--" + fl.Name
		}
		cases = append(cases, completionCase{key, append(append([]string{}, path...), "--"+fl.Name, "")})
	}
	cmd.LocalFlags().VisitAll(visit)
	cmd.InheritedFlags().VisitAll(visit)
	for _, sub := range cmd.Commands() {
		cases = append(cases, completionCases(sub)...)
	}
	return cases
}

// TestCompletionNeverListsFilesUnlessAPath walks every command, positional
// argument and flag through the hidden __complete command and checks that only
// a real path completes from the file system.
func TestCompletionNeverListsFilesUnlessAPath(t *testing.T) {
	t.Parallel()
	root := cli.Root(completionFactory(t, io.Discard))
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	seen := map[string]bool{}
	for _, tc := range completionCases(root) {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			_, got := complete(t, tc.args...)
			if want, ok := pathCompletions[tc.key]; ok {
				if got != want {
					t.Errorf("%s: directive %d, want %d", tc.key, got, want)
				}
				return
			}
			if got&cobra.ShellCompDirectiveNoFileComp == 0 ||
				got&(cobra.ShellCompDirectiveFilterFileExt|cobra.ShellCompDirectiveFilterDirs) != 0 {
				t.Errorf("%s: directive %d lets the shell list files", tc.key, got)
			}
		})
		seen[tc.key] = true
	}
	for key := range pathCompletions {
		if !seen[key] {
			t.Errorf("no command has %s", key)
		}
	}
	for _, want := range []string{"tui #0", "get #1", "status --namespace", "restore --tag", "completion bash #0"} {
		if !seen[want] {
			t.Errorf("the walk missed %s", want)
		}
	}
}

func TestCompletionOffersNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{cmdGet, kindRepo, ""}, []string{argsRepo}},
		{[]string{cmdGet, "backups", ""}, []string{argsApp01, argsApp02}},
		{[]string{cmdGet, ""}, []string{kindRepo, "sb"}},
		{[]string{cmdStatus, "-n", ""}, []string{argsNS}},
		{[]string{cmdStatus, "--context", ""}, []string{"lab-admin"}},
		{[]string{cmdStatus, "--cluster", ""}, []string{"lab"}},
		{[]string{cmdStatus, "--user", ""}, []string{"admin"}},
		{[]string{cmdTUI, ""}, nil},
		{[]string{cmdTUI, "--"}, []string{"--all-namespaces", "--mouse"}},
		{[]string{"validate", "a.yaml", ""}, nil},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			got, directive := complete(t, tt.args...)
			if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
				t.Errorf("directive %d allows files", directive)
			}
			for _, w := range tt.want {
				found := false
				for _, g := range got {
					if strings.SplitN(g, "\t", 2)[0] == w {
						found = true
					}
				}
				if !found {
					t.Errorf("completions %q are missing %q", got, w)
				}
			}
		})
	}
}

func TestCompletionWithoutAClusterListsNoFiles(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{cmdGet, kindRepo, ""}, {cmdStatus, ""}, {cmdRestore, ""}, {cmdStatus, "-n", ""}, {cmdRender, "app"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			c := argsClientBuilder(t).WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errForbidden
				},
			}).Build()
			var out bytes.Buffer
			f := cli.NewTestFactoryIn(argsNS, c, &out, io.Discard, nil)
			if err := cli.Execute(f, append([]string{cobra.ShellCompRequestCmd}, args...)...); err != nil {
				t.Fatal(err)
			}
			want := compNoFiles
			if args[0] == cmdRender {
				want = compManifests
			}
			if out.String() != want {
				t.Errorf("completions = %q, want %q", out.String(), want)
			}
		})
	}
}
