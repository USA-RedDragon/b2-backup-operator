package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

const (
	binaryName    = "b2bk"
	pluginDisplay = "kubectl " + binaryName
)

func TestDisplayName(t *testing.T) {
	tests := []struct {
		argv0 string
		want  string
	}{
		{binaryName, binaryName},
		{"/usr/local/bin/b2bk", binaryName},
		{"./bin/b2bk", binaryName},
		{"kubectl-b2bk", pluginDisplay},
		{"/opt/homebrew/bin/kubectl-b2bk", pluginDisplay},
		{"kubectl-b2bk.exe", pluginDisplay},

		{"kubectl-b2bk_thing", "kubectl b2bk-thing"},
	}

	for _, tt := range tests {
		if got := DisplayName(tt.argv0); got != tt.want {
			t.Errorf("DisplayName(%q) = %q, want %q", tt.argv0, got, tt.want)
		}
	}
}

func TestRootUsesInvokedIdentity(t *testing.T) {
	for argv0, wantUse := range map[string]string{
		binaryName:              binaryName,
		"/usr/bin/kubectl-b2bk": binaryName,
	} {
		cmd := New(testStreams(), argv0)
		if cmd.Use != wantUse {
			t.Errorf("argv0 %q: Use = %q, want %q", argv0, cmd.Use, wantUse)
		}
		if got, want := cmd.Annotations[cobra.CommandDisplayNameAnnotation], DisplayName(argv0); got != want {
			t.Errorf("argv0 %q: display name = %q, want %q", argv0, got, want)
		}
	}
}

func TestRootRegistersConnectionFlags(t *testing.T) {
	cmd := New(testStreams(), binaryName)
	for _, name := range []string{"namespace", "context", "kubeconfig"} {
		if cmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("missing persistent flag --%s", name)
		}
	}
	if f := cmd.PersistentFlags().ShorthandLookup("n"); f == nil || f.Name != "namespace" {
		t.Error("-n must be shorthand for --namespace")
	}
}

func TestAllNamespacesIsOptIn(t *testing.T) {
	if f := New(testStreams(), binaryName).PersistentFlags().ShorthandLookup("A"); f != nil {
		t.Error("-A must not be a persistent root flag")
	}

	f := NewFactory(testStreams())
	cmd := &cobra.Command{Use: "get"}
	f.AddAllNamespacesFlag(cmd)
	if flag := cmd.Flags().ShorthandLookup("A"); flag == nil || flag.Name != "all-namespaces" {
		t.Error("-A must be shorthand for --all-namespaces")
	}
}

var podStartCommands = []string{
	"shell", "exec", GroupSnapshots, "restore",
	"unlock", "check", "prune", "stats", "rotate-password",
}

func TestPodStartTimeoutIsOnEveryCommandThatStartsAPod(t *testing.T) {
	root := New(testStreams(), binaryName)
	if root.PersistentFlags().Lookup("pod-start-timeout") != nil {
		t.Error("--pod-start-timeout must not be persistent; most commands never start a pod")
	}

	want := make(map[string]bool, len(podStartCommands))
	for _, name := range podStartCommands {
		want[name] = true
	}

	for _, cmd := range root.Commands() {
		has := cmd.Flags().Lookup("pod-start-timeout") != nil
		switch {
		case want[cmd.Name()] && !has:
			t.Errorf("%s starts a pod, so it must accept --pod-start-timeout", cmd.Name())
		case !want[cmd.Name()] && has:
			t.Errorf("%s never starts a pod, so --pod-start-timeout only clutters its help", cmd.Name())
		}
		delete(want, cmd.Name())
	}
	for name := range want {
		t.Errorf("no command named %q is registered", name)
	}
}
