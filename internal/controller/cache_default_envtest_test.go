//go:build envtest

package controller_test

import (
	"context"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

func TestEnvtestCacheAccessModeDefault(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, b2backupv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{Name: "cache-default"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		cache *b2backupv1.CacheSpec
		want  corev1.PersistentVolumeAccessMode
	}{
		{name: "empty", cache: &b2backupv1.CacheSpec{}, want: corev1.ReadWriteOnce},
		{name: "explicit", cache: &b2backupv1.CacheSpec{AccessMode: corev1.ReadWriteMany}, want: corev1.ReadWriteMany},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := &b2backupv1.ScheduledBackup{
				Namespace: "cache-default", Name: tc.name,
				Spec: b2backupv1.ScheduledBackupSpec{
					RepositoryRef: corev1.LocalObjectReference{Name: "store"},
					Schedule:      "@daily",
					Sources:       []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeCNPG}},
					Cache:         tc.cache,
				},
			}
			if err := c.Create(ctx, sb); err != nil {
				t.Fatal(err)
			}
			if got := sb.Spec.Cache.AccessMode; got != tc.want {
				t.Errorf("spec.cache.accessMode = %q, want %q", got, tc.want)
			}
		})
	}
}
