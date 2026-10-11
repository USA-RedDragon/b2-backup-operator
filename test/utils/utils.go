// Package utils holds helpers for the e2e suite.
package utils

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultKindBinary  = "kind"
	defaultKindCluster = "b2-backup-operator-test-e2e"
)

var (
	kindKubeconfig string
	kindContext    string
)

// UseKindCluster checks that KUBECONFIG names a kubeconfig file whose current
// context is the e2e Kind cluster's and whose server is the one kind reports
// for that cluster, then pins every later Run to that file and context. It
// reads only the file and kind's own record of the cluster; it contacts no API
// server. Until it succeeds, Run refuses every command.
func UseKindCluster() error {
	cluster := kindClusterName()
	out, err := exec.Command(kindBinaryName(), "get", "kubeconfig", "--name", cluster).Output()
	if err != nil {
		return fmt.Errorf("kind has no cluster %q: %w", cluster, err)
	}
	server, err := kindServer(out, cluster)
	if err != nil {
		return err
	}
	path, context, err := VerifyKindKubeconfig(os.Getenv("KUBECONFIG"), cluster, server)
	if err != nil {
		return err
	}
	kindKubeconfig, kindContext = path, context
	return nil
}

func kindServer(kubeconfig []byte, cluster string) (string, error) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return "", fmt.Errorf("parsing kind's kubeconfig for %q: %w", cluster, err)
	}
	c, ok := cfg.Clusters["kind-"+cluster]
	if !ok || c.Server == "" {
		return "", fmt.Errorf("kind's kubeconfig has no server for cluster %q", cluster)
	}
	return c.Server, nil
}

// VerifyKindKubeconfig checks that path is a single kubeconfig file whose
// current context is kind-<cluster> and points at server. It returns the
// absolute path and the context name.
func VerifyKindKubeconfig(path, cluster, server string) (string, string, error) {
	if path == "" {
		return "", "", errors.New("KUBECONFIG is not set; refusing to fall back to the default kubeconfig")
	}
	if strings.ContainsRune(path, os.PathListSeparator) {
		return "", "", fmt.Errorf("KUBECONFIG %q lists several files; it must name only the Kind cluster's", path)
	}
	if server == "" {
		return "", "", errors.New("no Kind cluster server to compare against")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	cfg, err := clientcmd.LoadFromFile(abs)
	if err != nil {
		return "", "", fmt.Errorf("loading KUBECONFIG %q: %w", abs, err)
	}
	want := "kind-" + cluster
	if cfg.CurrentContext != want {
		return "", "", fmt.Errorf("KUBECONFIG %q has current context %q, not %q; refusing to run against it",
			abs, cfg.CurrentContext, want)
	}
	ctx, ok := cfg.Contexts[want]
	if !ok {
		return "", "", fmt.Errorf("KUBECONFIG %q has no context %q", abs, want)
	}
	c, ok := cfg.Clusters[ctx.Cluster]
	if !ok {
		return "", "", fmt.Errorf("KUBECONFIG %q has no cluster %q for context %q", abs, ctx.Cluster, want)
	}
	if c.Server != server {
		return "", "", fmt.Errorf("KUBECONFIG %q context %q points at %q, but Kind cluster %q is at %q",
			abs, want, c.Server, cluster, server)
	}
	return abs, want, nil
}

// Run executes cmd from the project directory and returns its combined output.
// It refuses to run anything before UseKindCluster has succeeded, runs every
// command with KUBECONFIG set to the Kind cluster's file, and gives kubectl
// that file and context explicitly.
func Run(cmd *exec.Cmd) (string, error) {
	if kindKubeconfig == "" || kindContext == "" {
		return "", fmt.Errorf("refusing to run %q: the Kind cluster's kubeconfig has not been verified",
			strings.Join(cmd.Args, " "))
	}
	dir, err := GetProjectDir()
	if err != nil {
		return "", err
	}
	cmd.Dir = dir

	if len(cmd.Args) > 0 && filepath.Base(cmd.Args[0]) == "kubectl" {
		cmd.Args = append([]string{cmd.Args[0], "--kubeconfig=" + kindKubeconfig, "--context=" + kindContext},
			cmd.Args[1:]...)
	}
	cmd.Env = append(os.Environ(), "GO111MODULE=on", "KUBECONFIG="+kindKubeconfig)
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// LoadImageToKindClusterWithName loads a local image into the Kind cluster.
func LoadImageToKindClusterWithName(name string) error {
	cmd := exec.Command(kindBinaryName(), "load", "docker-image", name, "--name", kindClusterName())
	_, err := Run(cmd)
	return err
}

func kindClusterName() string {
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok && v != "" {
		return v
	}
	return defaultKindCluster
}

func kindBinaryName() string {
	if v, ok := os.LookupEnv("KIND"); ok && v != "" {
		return v
	}
	return defaultKindBinary
}

// GetNonEmptyLines splits output into lines, dropping empty ones.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.SplitSeq(output, "\n")
	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir returns the repository root.
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	return strings.TrimSuffix(wd, "/test/e2e"), nil
}
