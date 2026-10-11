package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return clientBuilder(t, objs...).Build()
}

func clientBuilder(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		b2backupv1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(
			&b2backupv1.ScheduledBackup{}, &b2backupv1.Repository{}, &batchv1.Job{},
		)
}

func newRepo(ns string) *b2backupv1.Repository {
	name := testRepoName
	return &b2backupv1.Repository{
		Namespace: ns, Name: name, UID: types.UID("uid-repo-" + name),
	}
}

func newBackup(name, repoRef string) *b2backupv1.ScheduledBackup {
	ns := testNS
	return &b2backupv1.ScheduledBackup{
		Namespace: ns, Name: name, UID: types.UID("uid-sb-" + name),
		Spec: b2backupv1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: repoRef},
		},
	}
}

func TestResolve(t *testing.T) {
	c := newClient(t, newRepo(testNS), newBackup(testBackupName, testRepoName))
	ctx := context.Background()

	tests := []struct {
		name     string
		arg      string
		wantKind TargetKind
		wantName string
	}{
		{"bare name finds the backup", testBackupName, TargetScheduledBackup, testBackupName},
		{"bare name finds the repository", testRepoName, TargetRepository, testRepoName},
		{"sb prefix", "sb/web-files", TargetScheduledBackup, testBackupName},
		{"scheduledbackup prefix", subjectBackup, TargetScheduledBackup, testBackupName},
		{"backup prefix", "backup/web-files", TargetScheduledBackup, testBackupName},
		{"repo prefix", "repo/store", TargetRepository, testRepoName},
		{"repository prefix", subjectRepo, TargetRepository, testRepoName},
		{"prefixes are case insensitive", "SB/web-files", TargetScheduledBackup, testBackupName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(ctx, c, "prod", tt.arg)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.arg, err)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if got.Name() != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name(), tt.wantName)
			}
			if got.Namespace() != "prod" {
				t.Errorf("namespace = %q, want %q", got.Namespace(), "prod")
			}
		})
	}
}

func TestResolveRejectsAmbiguousName(t *testing.T) {
	c := newClient(t, newRepo(testNS), newBackup(testRepoName, testRepoName))

	_, err := Resolve(context.Background(), c, "prod", testRepoName)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
	for _, want := range []string{"sb/store", "repo/store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should suggest %q, got: %v", want, err)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	c := newClient(t, newRepo(testNS))
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		arg  string
		want error
	}{
		{"missing object", "nope", ErrTargetNotFound},
		{"missing qualified object", "repo/nope", ErrTargetNotFound},
		{"wrong kind for an existing name", "sb/store", ErrTargetNotFound},
		{"unknown prefix", "cronjob/store", ErrUnknownKind},
		{"empty name after prefix", "repo/", ErrTargetNotFound},
		{"empty arg", "", ErrTargetNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Resolve(ctx, c, "prod", tt.arg); !errors.Is(err, tt.want) {
				t.Errorf("Resolve(%q) = %v, want %v", tt.arg, err, tt.want)
			}
		})
	}
}

func TestResolveIsNamespaceScoped(t *testing.T) {
	c := newClient(t, newRepo(testNS))
	if _, err := Resolve(context.Background(), c, "staging", testRepoName); !errors.Is(err, ErrTargetNotFound) {
		t.Errorf("expected ErrTargetNotFound in another namespace, got %v", err)
	}
}

func TestRepositoryFor(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	c := newClient(t, newRepo(testNS), sb)

	got, err := RepositoryFor(context.Background(), c, sb)
	if err != nil {
		t.Fatalf("RepositoryFor: %v", err)
	}
	if got.Name != testRepoName {
		t.Errorf("repository = %q, want %q", got.Name, testRepoName)
	}

	dangling := newBackup("orphan", "gone")
	_, err = RepositoryFor(context.Background(), newClient(t, dangling), dangling)
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("expected ErrTargetNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "orphan") || !strings.Contains(err.Error(), "gone") {
		t.Errorf("error should name both objects, got: %v", err)
	}
}
