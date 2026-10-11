package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/USA-RedDragon/b2-backup-operator/internal/controller"
)

func env(t *testing.T, c client.Client, arg string, show bool) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runEnv(context.Background(), c, &out, &errOut, "prod", arg, show)
	return out.String(), errOut.String(), err
}

func TestEnvRedactsTheSecretsByDefault(t *testing.T) {
	r := readyRepo(testNS)
	c := newClient(t, r, credentialsSecret(r.SecretName()))

	out, errOut, err := env(t, c, "repo/"+testRepoName, false)
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	for _, leaked := range []string{"'pw'", "keysecret"} {
		if strings.Contains(out, leaked) {
			t.Errorf("%s leaked into output:\n%s", leaked, out)
		}
	}
	for _, want := range []string{
		"export RESTIC_REPOSITORY='" + testRepoURL + "'",
		"export RESTIC_PASSWORD='" + redacted + "'",
		"export AWS_ACCESS_KEY_ID='" + testKeyID + "'",
		"export AWS_SECRET_ACCESS_KEY='" + redacted + "'",
		"export AWS_DEFAULT_REGION='us-east-005'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}

	if !strings.Contains(errOut, "--show-password") {
		t.Errorf("expected a hint on stderr, got %q", errOut)
	}
	if strings.Contains(out, "#") {
		t.Errorf("stdout must contain only export lines:\n%s", out)
	}
}

func TestEnvShowPassword(t *testing.T) {
	r := readyRepo(testNS)
	c := newClient(t, r, credentialsSecret(r.SecretName()))

	out, errOut, err := env(t, c, "repo/"+testRepoName, true)
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}
	for _, want := range []string{
		"export RESTIC_PASSWORD='pw'",
		"export AWS_SECRET_ACCESS_KEY='keysecret'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
	if errOut != "" {
		t.Errorf("nothing was redacted, so there is nothing to hint about: %q", errOut)
	}
	if n := strings.Count(out, "export "); n != len(controller.SecretKeys) {
		t.Errorf("printed %d exports, want %d", n, len(controller.SecretKeys))
	}
}

func TestEnvViaScheduledBackup(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, r, sb, credentialsSecret(r.SecretName()))

	out, _, err := env(t, c, "sb/"+testBackupName, true)
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}
	if !strings.Contains(out, testRepoURL) {
		t.Errorf("expected the repository URL:\n%s", out)
	}
}

func TestEnvMissingSecret(t *testing.T) {
	c := newClient(t, readyRepo(testNS))
	if _, _, err := env(t, c, "repo/"+testRepoName, false); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("expected ErrTargetNotFound, got %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	if got, want := shellQuote(`a'b`), `'a'\''b'`; got != want {
		t.Errorf("shellQuote = %q, want %q", got, want)
	}
}
