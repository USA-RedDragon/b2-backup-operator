package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testServer = "https://127.0.0.1:40123"

func writeKubeconfig(t *testing.T, context, cluster, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	data := `apiVersion: v1
kind: Config
current-context: ` + context + `
contexts:
- name: ` + context + `
  context:
    cluster: ` + cluster + `
    user: ` + context + `
clusters:
- name: ` + cluster + `
  cluster:
    server: ` + server + `
users:
- name: ` + context + `
  user:
    token: x
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyKindKubeconfig(t *testing.T) {
	const cluster = "b2-backup-operator-test-e2e"
	good := writeKubeconfig(t, "kind-"+cluster, "kind-"+cluster, testServer)

	path, context, err := VerifyKindKubeconfig(good, cluster, testServer)
	if err != nil {
		t.Fatalf("kind kubeconfig refused: %v", err)
	}
	if path != good || context != "kind-"+cluster {
		t.Fatalf("got %q %q", path, context)
	}

	cases := map[string]struct {
		path, server, want string
	}{
		"unset":           {"", testServer, "not set"},
		"list":            {good + string(os.PathListSeparator) + good, testServer, "several files"},
		"missing":         {filepath.Join(t.TempDir(), "nope"), testServer, "loading"},
		"no kind server":  {good, "", "no Kind cluster server"},
		"other server":    {good, "https://127.0.0.1:40124", "points at"},
		"non-kind":        {writeKubeconfig(t, "admin@home", "home", "https://192.0.2.1:6443"), testServer, "admin@home"},
		"other kind":      {writeKubeconfig(t, "kind-other", "kind-other", testServer), testServer, "kind-other"},
		"renamed cluster": {writeKubeconfig(t, "kind-"+cluster, "home", "https://192.0.2.1:6443"), testServer, "points at"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := VerifyKindKubeconfig(tc.path, cluster, tc.server)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestKindServer(t *testing.T) {
	data, err := os.ReadFile(writeKubeconfig(t, "kind-c", "kind-c", testServer))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := kindServer(data, "c"); err != nil || got != testServer {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := kindServer(data, "d"); err == nil {
		t.Fatal("found a server for a cluster kind did not report")
	}
}

func TestRunRefusesBeforeVerification(t *testing.T) {
	kindKubeconfig, kindContext = "", ""
	marker := filepath.Join(t.TempDir(), "ran")
	if _, err := Run(exec.Command("touch", marker)); err == nil {
		t.Fatal("Run ran a command before the Kind kubeconfig was verified")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the command ran: %v", err)
	}
}

func TestRunPinsKubectlToKind(t *testing.T) {
	kindKubeconfig, kindContext = "/k/e2e.kubeconfig", "kind-c"
	t.Cleanup(func() { kindKubeconfig, kindContext = "", "" })
	cmd := exec.Command(filepath.Join(t.TempDir(), "kubectl"), "delete", "ns", "x")
	_, _ = Run(cmd)
	want := []string{cmd.Args[0], "--kubeconfig=/k/e2e.kubeconfig", "--context=kind-c", "delete", "ns", "x"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("args %q, want %q", cmd.Args, want)
	}
	if cmd.Env[len(cmd.Env)-1] != "KUBECONFIG=/k/e2e.kubeconfig" {
		t.Fatalf("env ends with %q", cmd.Env[len(cmd.Env)-1])
	}
}

func TestE2ERunsCommandsOnlyThroughRun(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "e2e", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no e2e sources found: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range []string{".CombinedOutput()", ".Output()", ".Run()", ".Start()"} {
			if strings.Contains(string(data), call) {
				t.Errorf("%s calls %s; run commands through utils.Run so they stay on the Kind cluster", f, call)
			}
		}
	}
}
