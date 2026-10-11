package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func setSuspend(t *testing.T, c client.Client, arg string, suspend bool) string {
	t.Helper()
	past := "resumed"
	if suspend {
		past = "suspended"
	}
	var buf bytes.Buffer
	if err := runSetSuspend(context.Background(), c, &buf, "prod", arg, suspend, past); err != nil {
		t.Fatalf("runSetSuspend: %v", err)
	}
	return buf.String()
}

func TestSuspendAndResumeScheduledBackup(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, sb)
	ctx := context.Background()

	if out := setSuspend(t, c, "sb/"+testBackupName, true); !strings.Contains(out, "suspended") {
		t.Errorf("unexpected output: %q", out)
	}

	var got b2backupv1.ScheduledBackup
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: testBackupName}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Suspend {
		t.Fatal("spec.suspend was not set")
	}

	setSuspend(t, c, "sb/"+testBackupName, false)
	if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: testBackupName}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Suspend {
		t.Error("spec.suspend was not cleared")
	}
}

func TestSuspendRepository(t *testing.T) {
	c := newClient(t, newRepo(testNS))
	setSuspend(t, c, "repo/"+testRepoName, true)

	var got b2backupv1.Repository
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: testRepoName}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Suspend {
		t.Error("spec.suspend was not set on the repository")
	}
}

func TestSuspendIsIdempotent(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	sb.Spec.Suspend = true
	c := newClient(t, sb)

	out := setSuspend(t, c, "sb/"+testBackupName, true)
	if !strings.Contains(out, "already suspended") {
		t.Errorf("expected an already-suspended notice, got %q", out)
	}
}
