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

func TestCRDScheduledBackupValidation(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "backups")

	base := func() *b2backupv1.ScheduledBackup {
		return &b2backupv1.ScheduledBackup{
			Namespace: "backups",
			Spec: b2backupv1.ScheduledBackupSpec{
				RepositoryRef: corev1.LocalObjectReference{Name: resticName},
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
			name: "env swapping the S3 credentials",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Env = map[string]string{"AWS_ACCESS_KEY_ID": "id-of-another-key", "AWS_SECRET_ACCESS_KEY": "another-key"}
			},
			wantErr: true,
		},
		{
			name: "env changing the region",
			mutate: func(s *b2backupv1.ScheduledBackup) {
				s.Spec.Env = map[string]string{"AWS_DEFAULT_REGION": "us-west-004"}
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
	return rawObject("Repository", name, namespace, map[string]any{
		fieldMode: "Bucket", fieldBucket: "acme-" + name, "interval": interval,
	})
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
		"repositoryRef": map[string]any{"name": resticName},
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

const (
	fieldMode   = "mode"
	fieldBucket = "bucket"
	fieldPath   = "path"
)

func validRepository(name, namespace string) *b2backupv1.Repository {
	return &b2backupv1.Repository{
		Name: name, Namespace: namespace,
		Spec: b2backupv1.RepositorySpec{
			Mode:   b2backupv1.StorageModePrefix,
			Bucket: "acme-shared",
			Path:   "restic/" + name,
		},
	}
}

func TestCRDRepositoryValidation(t *testing.T) {
	c := startEnv(t)
	mustCreateNamespace(t, c, "repos")

	tests := []struct {
		name    string
		mutate  func(*b2backupv1.Repository)
		wantErr string
	}{
		{name: "a valid Prefix repository", mutate: func(*b2backupv1.Repository) {}},
		{name: "a valid Bucket repository", mutate: func(r *b2backupv1.Repository) {
			r.Spec.Mode = b2backupv1.StorageModeBucket
			r.Spec.Path = ""
		}},
		{name: "a default path", mutate: func(r *b2backupv1.Repository) { r.Spec.Path = "" }},
		{name: "no mode", mutate: func(r *b2backupv1.Repository) { r.Spec.Mode = "" }, wantErr: fieldMode},
		{name: "an unknown mode", mutate: func(r *b2backupv1.Repository) { r.Spec.Mode = "Folder" }, wantErr: fieldMode},
		{name: "no bucket", mutate: func(r *b2backupv1.Repository) { r.Spec.Bucket = "" }, wantErr: fieldBucket},
		{name: "a short bucket name", mutate: func(r *b2backupv1.Repository) { r.Spec.Bucket = "abcde" }, wantErr: fieldBucket},
		{name: "the longest bucket name", mutate: func(r *b2backupv1.Repository) {
			r.Spec.Bucket = strings.Repeat("a", 63)
		}},
		{name: "a long bucket name", mutate: func(r *b2backupv1.Repository) {
			r.Spec.Bucket = strings.Repeat("a", 64)
		}, wantErr: fieldBucket},
		{name: "an uppercase bucket name", mutate: func(r *b2backupv1.Repository) { r.Spec.Bucket = "Acme-Shared" }, wantErr: fieldBucket},
		{name: "a bucket name with a dot", mutate: func(r *b2backupv1.Repository) { r.Spec.Bucket = "acme.shared" }, wantErr: fieldBucket},
		{name: "a bucket name starting with a dash", mutate: func(r *b2backupv1.Repository) { r.Spec.Bucket = "-acme-shared" }, wantErr: fieldBucket},
		{name: "a reserved bucket name", mutate: func(r *b2backupv1.Repository) { r.Spec.Bucket = "b2-backups" }, wantErr: "reserved"},
		{name: "a path with a leading slash", mutate: func(r *b2backupv1.Repository) { r.Spec.Path = "/restic/app" }, wantErr: fieldPath},
		{name: "a path with a trailing slash", mutate: func(r *b2backupv1.Repository) { r.Spec.Path = "restic/app/" }, wantErr: fieldPath},
		{name: "a path with an empty segment", mutate: func(r *b2backupv1.Repository) { r.Spec.Path = "restic//app" }, wantErr: fieldPath},
		{name: "a path climbing out", mutate: func(r *b2backupv1.Repository) { r.Spec.Path = "restic/../app" }, wantErr: ". or .."},
		{name: "a path with a dot segment", mutate: func(r *b2backupv1.Repository) { r.Spec.Path = "./app" }, wantErr: ". or .."},
		{name: "a path with dots inside a name", mutate: func(r *b2backupv1.Repository) { r.Spec.Path = "restic/my.app..v2" }},
		{name: "zero lifecycle days", mutate: func(r *b2backupv1.Repository) {
			r.Spec.Lifecycle = &b2backupv1.LifecycleSpec{DaysFromHidingToDeleting: ptr.To(int32(0))}
		}, wantErr: "daysFromHidingToDeleting"},
		{name: "secretName naming the seed", mutate: func(r *b2backupv1.Repository) {
			r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: seedSecret, Key: KeyResticPassword}
			r.Spec.SecretName = seedSecret
		}, wantErr: "the operator never writes to the seed"},
		{name: "secretName naming the credentials", mutate: func(r *b2backupv1.Repository) {
			r.Spec.CredentialsSecretRef = &corev1.LocalObjectReference{Name: teamSecret}
			r.Spec.SecretName = teamSecret
		}, wantErr: "credentialsSecretRef"},
		{name: "distinct Secrets", mutate: func(r *b2backupv1.Repository) {
			r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: seedSecret, Key: KeyResticPassword}
			r.Spec.CredentialsSecretRef = &corev1.LocalObjectReference{Name: teamSecret}
			r.Spec.SecretName = "restic-credentials"
		}},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := validRepository(fmt.Sprintf("repo-%d", i), "repos")
			tt.mutate(repo)
			err := c.Create(context.Background(), repo)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("rejected: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Error("accepted, want rejection")
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("rejected for the wrong reason: %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestCRDRepositoryLocationIsImmutable(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "immutable")

	tests := []struct {
		name    string
		create  func(*b2backupv1.Repository)
		update  func(*b2backupv1.Repository)
		wantErr bool
	}{
		{name: fieldMode, update: func(r *b2backupv1.Repository) { r.Spec.Mode = b2backupv1.StorageModeBucket }, wantErr: true},
		{name: fieldBucket, update: func(r *b2backupv1.Repository) { r.Spec.Bucket = "acme-other" }, wantErr: true},
		{name: "changing the path", update: func(r *b2backupv1.Repository) { r.Spec.Path = "restic/moved" }, wantErr: true},
		{name: "removing the path", update: func(r *b2backupv1.Repository) { r.Spec.Path = "" }, wantErr: true},
		{
			name:    "adding a path to a defaulted one",
			create:  func(r *b2backupv1.Repository) { r.Spec.Path = "" },
			update:  func(r *b2backupv1.Repository) { r.Spec.Path = "immutable/added" },
			wantErr: true,
		},
		{name: "an unrelated field", update: func(r *b2backupv1.Repository) { r.Spec.Suspend = true }},
		{name: "the lifecycle rule", update: func(r *b2backupv1.Repository) {
			r.Spec.Lifecycle = &b2backupv1.LifecycleSpec{DaysFromHidingToDeleting: ptr.To(int32(30))}
		}},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := validRepository(fmt.Sprintf("loc-%d", i), "immutable")
			if tt.create != nil {
				tt.create(repo)
			}
			if err := c.Create(ctx, repo); err != nil {
				t.Fatal(err)
			}
			var got b2backupv1.Repository
			if err := c.Get(ctx, client.ObjectKeyFromObject(repo), &got); err != nil {
				t.Fatal(err)
			}
			tt.update(&got)
			err := c.Update(ctx, &got)
			if tt.wantErr && err == nil {
				t.Error("the update was accepted, want rejection")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("the update was rejected: %v", err)
			}
		})
	}
}
