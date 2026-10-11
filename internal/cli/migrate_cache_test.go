package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestMigrateKeepsTheCacheClaimsAccessMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		volume     string
		wantClass  string
		wantAccess string
		wantCache  bool
	}{
		{name: "an RWX class and mode", wantCache: true, wantClass: "ceph-filesystem", wantAccess: "ReadWriteMany",
			volume: "        storageClass: ceph-filesystem\n        accessMode: ReadWriteMany\n"},
		{name: "a class alone", wantCache: true, wantClass: "longhorn-nvme",
			volume: "        storageClass: longhorn-nvme\n"},
		{name: "a mode alone", wantCache: true, wantAccess: "ReadWriteOnce",
			volume: "        accessMode: ReadWriteOnce\n"},
		{name: "neither", volume: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := strings.NewReplacer("SCHEDULE", "0 3 * * *", "TIMEZONE", "", "TZENV", "").Replace(zoneRelease)
			body += "      cache:\n        type: persistentVolumeClaim\n        size: 1Gi\n" + tc.volume
			dir := filepath.Join(t.TempDir(), "resources", "restic")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "helmrelease.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runB2bk(t, "migrate", dir, "--repository", zoneRepository)
			if err != nil {
				t.Fatalf("migrate: %v\n%s", err, out)
			}
			cache := generatedCache(t, out)
			switch {
			case !tc.wantCache && cache != nil:
				t.Errorf("spec.cache = %+v, want none", *cache)
			case tc.wantCache && cache == nil:
				t.Fatalf("no spec.cache in:\n%s", out)
			case tc.wantCache && (cache.StorageClass != tc.wantClass || cache.AccessMode != tc.wantAccess):
				t.Errorf("spec.cache = %+v, want storageClass %q accessMode %q", *cache, tc.wantClass, tc.wantAccess)
			}
		})
	}
}

type migratedCache struct {
	StorageClass string `json:"storageClass"`
	AccessMode   string `json:"accessMode"`
}

func generatedCache(t *testing.T, out string) *migratedCache {
	t.Helper()
	for doc := range strings.SplitSeq(out, "\n---\n") {
		var obj struct {
			Kind string `json:"kind"`
			Spec struct {
				Cache *migratedCache `json:"cache"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("reading the migrate output: %v", err)
		}
		if obj.Kind == kindBackup {
			return obj.Spec.Cache
		}
	}
	t.Fatalf("no ScheduledBackup in:\n%s", out)
	return nil
}
