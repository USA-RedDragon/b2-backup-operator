//go:build envtest

package controller

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const (
	existingClaimName = "data"
	seedNamespace     = "seed"
	seedSecretName    = "seed"
)

// envClient is a single control plane shared by every test in this file. A
// control plane costs about four seconds to start and none of these tests
// mutate anything the others read, so they share one.
var envClient client.Client

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting envtest (CRDs may have an invalid CEL rule): %v\n", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, b2backupv1.AddToScheme} {
		if err := add(scheme); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if envClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func startEnv(t *testing.T) client.Client {
	t.Helper()
	return envClient
}

func mustCreateNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	if err := c.Create(context.Background(), &corev1.Namespace{Name: name}); err != nil {
		t.Fatal(err)
	}
}

func TestCRDRejectsUnparseableDurations(t *testing.T) {
	c := startEnv(t)
	mustCreateNamespace(t, c, "durations")

	tests := []struct {
		value   string
		wantErr bool
	}{
		{"1h", false},
		{"90m", false},
		{"1h30m", false},
		{"0", false},
		{"1 hour", true},
		{"hourly", true},
		{"1d", true},
		{"", true},
	}

	for i, tt := range tests {
		obj := repositoryWithRawInterval(fmt.Sprintf("dur-%d", i), "durations", tt.value)
		err := c.Create(context.Background(), obj)
		if tt.wantErr && err == nil {
			t.Errorf("interval %q was accepted, want rejection", tt.value)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("interval %q was rejected: %v", tt.value, err)
		}
	}
}

func TestCRDExistingRepositoryIDIsImmutable(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "immutable")

	adopted := &b2backupv1.Repository{
		Name: adoptedName, Namespace: "immutable",
		Spec: b2backupv1.RepositorySpec{
			ExistingRepositoryID: adoptedRepoID,
			PasswordSecretRef:    &corev1.SecretKeySelector{Name: seedSecretName, Key: KeyResticPassword},
		},
	}
	if err := c.Create(ctx, adopted); err != nil {
		t.Fatal(err)
	}

	t.Run("cannot be changed", func(t *testing.T) {
		var got b2backupv1.Repository
		if err := c.Get(ctx, client.ObjectKeyFromObject(adopted), &got); err != nil {
			t.Fatal(err)
		}
		got.Spec.ExistingRepositoryID = "e5f6a7b8"
		if err := c.Update(ctx, &got); err == nil {
			t.Error("existingRepositoryID was changed")
		}
	})

	t.Run("cannot be removed", func(t *testing.T) {
		var got b2backupv1.Repository
		if err := c.Get(ctx, client.ObjectKeyFromObject(adopted), &got); err != nil {
			t.Fatal(err)
		}
		got.Spec.ExistingRepositoryID = ""
		if err := c.Update(ctx, &got); err == nil {
			t.Error("existingRepositoryID was removed, which the old field-level rule allowed")
		}
	})

	t.Run("cannot be added after a repository was created", func(t *testing.T) {
		created := &b2backupv1.Repository{
			Name: "created", Namespace: "immutable",
			Spec: b2backupv1.RepositorySpec{Region: "us"},
		}
		if err := c.Create(ctx, created); err != nil {
			t.Fatal(err)
		}
		created.Status.RepositoryID = otherRepoID
		if err := c.Status().Update(ctx, created); err != nil {
			t.Fatal(err)
		}

		var got b2backupv1.Repository
		if err := c.Get(ctx, client.ObjectKeyFromObject(created), &got); err != nil {
			t.Fatal(err)
		}
		got.Spec.ExistingRepositoryID = adoptedRepoID
		got.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: seedSecretName, Key: KeyResticPassword}
		if err := c.Update(ctx, &got); err == nil {
			t.Error("adopted a different repository after one had already been created")
		}
	})
}

func TestCRDRejectsSecretNameEqualToSeed(t *testing.T) {
	c := startEnv(t)
	mustCreateNamespace(t, c, seedNamespace)

	repo := &b2backupv1.Repository{
		Name: "clash", Namespace: seedNamespace,
		Spec: b2backupv1.RepositorySpec{
			ExistingRepositoryID: adoptedRepoID,
			PasswordSecretRef:    &corev1.SecretKeySelector{Name: "restic-envs", Key: KeyResticPassword},
			SecretName:           "restic-envs",
		},
	}
	if err := c.Create(context.Background(), repo); err == nil {
		t.Error("secretName was allowed to name the seed Secret")
	}

	repo.Name = "fine"
	repo.Spec.SecretName = "restic-b2"
	if err := c.Create(context.Background(), repo); err != nil {
		t.Errorf("a distinct secretName was rejected: %v", err)
	}
}

func TestCRDScheduledBackupValidation(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "backups")

	base := func() *b2backupv1.ScheduledBackup {
		return &b2backupv1.ScheduledBackup{
			Namespace: "backups",
			Spec: b2backupv1.ScheduledBackupSpec{
				RepositoryRef: corev1.LocalObjectReference{Name: "restic"},
				Schedule:      hourlySchedule,
				Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*b2backupv1.ScheduledBackup)
		wantErr bool
	}{
		{
			name: "files source without a volume",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}
			},
			wantErr: true,
		},
		{
			name: "files source with a volume",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles}}
				s.Spec.Volume = &b2backupv1.VolumeSpec{ExistingClaim: existingClaimName}
			},
		},
		{
			name:    "no sources and no script",
			mutate:  func(s *b2backupv1.ScheduledBackup) { s.Spec.Sources = nil },
			wantErr: true,
		},
		{
			name: "a script instead of sources",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Sources = nil
				s.Spec.Script = testScript
			},
		},
		{
			name:    "both sources and a script",
			mutate:  func(s *b2backupv1.ScheduledBackup) { s.Spec.Script = testScript },
			wantErr: true,
		},
		{
			name:    "retention with nothing set",
			mutate:  func(s *b2backupv1.ScheduledBackup) { s.Spec.Retention = &b2backupv1.Retention{} },
			wantErr: true,
		},
		{
			name: "retention with one tier",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Retention = &b2backupv1.Retention{Daily: ptr.To(int32(90))}
			},
		},
		{
			name: "mariadb without connection details",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Database = &b2backupv1.DatabaseSpec{Engine: b2backupv1.DatabaseEngineMariaDB}
			},
			wantErr: true,
		},
		{
			name: "mariadb with connection details",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Database = &b2backupv1.DatabaseSpec{
					Engine: b2backupv1.DatabaseEngineMariaDB,
					Host:   "mariadb", Name: "appdb", User: "appdb",
				}
			},
		},
		{
			name: "exclude on a database source",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Sources = []b2backupv1.BackupSource{
					{Type: b2backupv1.SourceTypeCNPG, Exclude: []string{"nope"}},
				}
			},
			wantErr: true,
		},
		{
			name: "database name on a files source",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Sources = []b2backupv1.BackupSource{
					{Type: b2backupv1.SourceTypeFiles, Database: "nope"},
				}
				s.Spec.Volume = &b2backupv1.VolumeSpec{ExistingClaim: existingClaimName}
			},
			wantErr: true,
		},
		{
			name: "a path on a database source",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Sources = []b2backupv1.BackupSource{
					{Type: b2backupv1.SourceTypeCNPG, Path: "/srv/app"},
				}
			},
			wantErr: true,
		},
		{
			name: "extraArgs on a files source",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Sources = []b2backupv1.BackupSource{
					{Type: b2backupv1.SourceTypeFiles, ExtraArgs: []string{"--single-transaction"}},
				}
				s.Spec.Volume = &b2backupv1.VolumeSpec{ExistingClaim: existingClaimName}
			},
			wantErr: true,
		},
		{
			name: "volume mounted anywhere but the cache",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Volume = &b2backupv1.VolumeSpec{
					ExistingClaim: existingClaimName, MountPath: "/srv/data",
				}
			},
		},
		{
			name: "volume mounted over the cache",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Volume = &b2backupv1.VolumeSpec{ExistingClaim: existingClaimName, MountPath: "/cache"}
			},
			wantErr: true,
		},
		{
			name: "unknown concurrency policy",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.ConcurrencyPolicy = "Sometimes"
			},
			wantErr: true,
		},
		{
			name: "env pointing the backup at another repository",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Env = map[string]string{"RESTIC_REPOSITORY": "rest:https://attacker/"}
			},
			wantErr: true,
		},
		{
			name: "env replacing the encryption key",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Env = map[string]string{"RESTIC_PASSWORD": "pwn"}
			},
			wantErr: true,
		},
		{
			name: "env the operator does not set",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Env = map[string]string{"RESTIC_GROUP_BY": "tags", "APP_ENV": "production"}
			},
		},
		{
			name: "a retryLock the operator can parse",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.RetryLock = &metav1.Duration{Duration: 90 * time.Minute}
			},
		},
		{
			name:    "a resticHost that is not a host name",
			mutate:  func(s *b2backupv1.ScheduledBackup) { s.Spec.ResticHost = "acme/prod" },
			wantErr: true,
		},
		{
			name:   "a pinned resticHost",
			mutate: func(s *b2backupv1.ScheduledBackup) { s.Spec.ResticHost = "acme-prod" },
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := base()
			sb.Name = fmt.Sprintf("sb-%d", i)
			tt.mutate(sb)

			err := c.Create(ctx, sb)
			if tt.wantErr && err == nil {
				t.Error("accepted, want rejection")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}

func rawObject(kind, name, namespace string, spec map[string]any) client.Object {
	obj := &unstructured.Unstructured{}
	obj.SetUnstructuredContent(map[string]any{
		"apiVersion": b2backupv1.SchemeGroupVersion.String(),
		"kind":       kind,
		"spec":       spec,
	})
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return obj
}

func repositoryWithRawInterval(name, namespace, interval string) client.Object {
	return rawObject("Repository", name, namespace, map[string]any{"interval": interval})
}

func TestCRDSamplesAreValid(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "myapp-prod")
	mustCreateNamespace(t, c, "otherapp-prod")

	matches, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "b2backup_v1_*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no sample manifests found")
	}

	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, doc := range strings.Split(string(raw), "\n---\n") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
				t.Errorf("%s doc %d: %v", filepath.Base(path), i, err)
				continue
			}
			if err := c.Create(ctx, obj); err != nil {
				t.Errorf("%s doc %d (%s/%s) was rejected: %v",
					filepath.Base(path), i, obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}
}

// scheduledBackupRaw builds a ScheduledBackup through unstructured, so a test
// can send a spec the Go types cannot express: an empty sources list, which
// omitempty drops, or a retryLock that is not a Go duration at all.
func scheduledBackupRaw(name, namespace string, spec map[string]any) client.Object {
	full := map[string]any{
		"repositoryRef": map[string]any{"name": "restic"},
		"schedule":      hourlySchedule,
		"sources":       []any{map[string]any{"type": "cnpg"}},
	}
	maps.Copy(full, spec)
	return rawObject(kindScheduledBackup, name, namespace, full)
}

func TestCRDRejectsAnEmptySourcesList(t *testing.T) {
	c := startEnv(t)
	mustCreateNamespace(t, c, "rawspec")

	obj := scheduledBackupRaw("empty-sources", "rawspec", map[string]any{"sources": []any{}})
	err := c.Create(context.Background(), obj)
	if err == nil {
		t.Fatal("a ScheduledBackup with sources: [] was accepted, want MinItems to reject it")
	}
	if !strings.Contains(err.Error(), "should have at least 1 items") {
		t.Errorf("rejected for the wrong reason: %v, want the MinItems message", err)
	}
}

func TestCRDRejectsUnparseableRetryLock(t *testing.T) {
	c := startEnv(t)
	mustCreateNamespace(t, c, "retrylock")

	tests := []struct {
		value   string
		wantErr bool
	}{
		{"90m", false},
		{"1h30m", false},
		{"0", false},
		{"1 hour", true},
		{"1d", true},
		{"", true},
	}

	for i, tt := range tests {
		obj := scheduledBackupRaw(fmt.Sprintf("lock-%d", i), "retrylock",
			map[string]any{"retryLock": tt.value})
		err := c.Create(context.Background(), obj)
		switch {
		case tt.wantErr && err == nil:
			t.Errorf("retryLock %q was accepted, want rejection", tt.value)
		case tt.wantErr && !strings.Contains(err.Error(), "Go duration"):
			t.Errorf("retryLock %q was rejected for the wrong reason: %v", tt.value, err)
		case !tt.wantErr && err != nil:
			t.Errorf("retryLock %q was rejected: %v", tt.value, err)
		}
	}
}

func TestCRDAdoptionRequiresAPasswordSecretRef(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "adoption")

	repo := &b2backupv1.Repository{
		Name: "no-password", Namespace: "adoption",
		Spec: b2backupv1.RepositorySpec{ExistingRepositoryID: adoptedRepoID},
	}
	err := c.Create(ctx, repo)
	if err == nil {
		t.Fatal("adopted a repository with no passwordSecretRef; its snapshots would be unreadable")
	}
	if !strings.Contains(err.Error(), "passwordSecretRef is required when adopting") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}

	repo.Spec.PasswordSecretRef = &corev1.SecretKeySelector{
		Name: seedSecretName, Key: KeyResticPassword,
	}
	if err := c.Create(ctx, repo); err != nil {
		t.Errorf("adoption with a passwordSecretRef was rejected: %v", err)
	}

	byName := &b2backupv1.Repository{
		Name: "by-name", Namespace: "adoption",
		Spec: b2backupv1.RepositorySpec{RepositoryName: legacyName},
	}
	if err := c.Create(ctx, byName); err != nil {
		t.Errorf("a Repository without existingRepositoryID was rejected: %v; a name that "+
			"turns out to match an existing repository is adoption too, but CEL cannot see "+
			"the BorgBase side, and requiring passwordSecretRef here would break every new "+
			"Repository. reconcileSecret refuses at reconcile time instead", err)
	}
}

func TestCRDImmutabilityAllowsUnchangedUpdates(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "unchanged")

	adopted := &b2backupv1.Repository{
		Name: adoptedName, Namespace: "unchanged",
		Spec: b2backupv1.RepositorySpec{
			ExistingRepositoryID: adoptedRepoID,
			PasswordSecretRef:    &corev1.SecretKeySelector{Name: seedSecretName, Key: KeyResticPassword},
		},
	}
	if err := c.Create(ctx, adopted); err != nil {
		t.Fatal(err)
	}

	created := &b2backupv1.Repository{
		Name: "created", Namespace: "unchanged",
		Spec: b2backupv1.RepositorySpec{Region: "us"},
	}
	if err := c.Create(ctx, created); err != nil {
		t.Fatal(err)
	}
	created.Status.RepositoryID = otherRepoID
	if err := c.Status().Update(ctx, created); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		obj  client.Object
	}{
		{"an adopted repository", adopted},
		{"a created repository", created},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got b2backupv1.Repository
			if err := c.Get(ctx, client.ObjectKeyFromObject(tt.obj), &got); err != nil {
				t.Fatal(err)
			}
			got.Spec.Suspend = true
			if err := c.Update(ctx, &got); err != nil {
				t.Errorf("an update leaving existingRepositoryID alone was rejected: %v", err)
			}
		})
	}
}
