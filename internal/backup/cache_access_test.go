package backup_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	"github.com/USA-RedDragon/b2-backup-operator/internal/backup"
)

func TestCacheClaimIsReadWriteOnceByDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cache *b2backupv1.CacheSpec
		want  corev1.PersistentVolumeAccessMode
	}{
		{name: "no cache settings", want: corev1.ReadWriteOnce},
		{name: "a class but no mode", cache: &b2backupv1.CacheSpec{StorageClass: "longhorn-nvme"},
			want: corev1.ReadWriteOnce},
		{name: "an explicit ReadWriteMany", cache: &b2backupv1.CacheSpec{AccessMode: corev1.ReadWriteMany},
			want: corev1.ReadWriteMany},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb := &b2backupv1.ScheduledBackup{
				Namespace: "media", Name: "media-files",
				Spec: b2backupv1.ScheduledBackupSpec{Cache: tc.cache},
			}
			pvc, err := backup.BuildCachePVC(sb, backup.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != tc.want {
				t.Errorf("accessModes = %v, want [%s]", pvc.Spec.AccessModes, tc.want)
			}
		})
	}
}
