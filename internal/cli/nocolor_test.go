package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"
	"github.com/USA-RedDragon/b2-backup-operator/internal/tui"
)

// TestCLIWritesNoEscapes checks that the plain commands never color their
// output, with NO_COLOR set or on a terminal that asks for color.
func TestCLIWritesNoEscapes(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "helm.toolkit.fluxcd.io/v2", Kind: "HelmRelease", Name: testBackupName,
		UID: "uid-helmrelease", Controller: ptr.To(true),
	}}
	c := newClient(t, r, sb, credentialsSecret(r.SecretName()), cj, boundCache(sb))

	commands := []string{
		"doctor sb/" + testBackupName,
		"doctor",
		"get",
		"get -o wide",
		"status sb/" + testBackupName,
		"status repository/" + testRepoName,
		"statsu",
		"--help",
		"restore --help",
	}
	for _, env := range []map[string]string{
		{tui.EnvNoColor: "1", "TERM": "xterm-256color", "COLORTERM": "truecolor"},
		{tui.EnvNoColor: "", "TERM": "xterm-256color", "COLORTERM": "truecolor", tui.EnvCLIColorForce: "1"},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		for _, line := range commands {
			args := strings.Fields(line)
			var out, errOut bytes.Buffer
			f := NewTestFactoryIn(testNS, c, &out, &errOut, func(runner.Options) (RestoreExec, error) {
				t.Fatal("a read-only command started a pod")
				return nil, io.EOF
			})
			err := Execute(f, args...)
			text := out.String() + errOut.String()
			if err != nil {
				text += err.Error()
			}
			if strings.TrimSpace(text) == "" {
				t.Errorf("b2bk %s printed nothing", strings.Join(args, " "))
			}
			if i := strings.IndexByte(text, 0x1b); i >= 0 {
				t.Errorf("b2bk %s with %v wrote an escape sequence at byte %d: %q",
					strings.Join(args, " "), env, i, text[i:min(len(text), i+12)])
			}
		}
	}
}
