package backup

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

const (
	dropAll      = corev1.Capability("ALL")
	appDir       = "app"
	dataClaim    = "data"
	dataMount    = "/data"
	scratchMount = "/backup"
	configClaim  = "config"
	offMount     = "on none of"
)

func filesBackup(volumes []b2backupv1.VolumeSpec, scratch *b2backupv1.ScratchSpec, paths ...string) *b2backupv1.ScheduledBackup {
	return testBackup(func(sb *b2backupv1.ScheduledBackup) {
		sb.Spec.Volumes = volumes
		sb.Spec.Scratch = scratch
		sb.Spec.Sources = []b2backupv1.BackupSource{{Type: b2backupv1.SourceTypeFiles, Paths: paths}}
	})
}

func TestValidate(t *testing.T) {
	t.Parallel()
	data := b2backupv1.VolumeSpec{ExistingClaim: appDir, MountPath: dataMount}
	blobs := b2backupv1.VolumeSpec{ExistingClaim: "blobs"}
	tests := []struct {
		name    string
		sb      *b2backupv1.ScheduledBackup
		wantErr string
	}{
		{"a relative path on the first volume", filesBackup([]b2backupv1.VolumeSpec{data}, nil, appDir), ""},
		{"paths on two volumes", filesBackup([]b2backupv1.VolumeSpec{data, blobs}, nil, dataMount, "/blobs"), ""},
		{"a path on scratch space", filesBackup(nil, &b2backupv1.ScratchSpec{MountPath: scratchMount}, "/backup/wx.db"), ""},
		{"a path on no mount", filesBackup([]b2backupv1.VolumeSpec{data}, nil, "/etc"), offMount},
		{"a relative path escaping the volume", filesBackup([]b2backupv1.VolumeSpec{data}, nil, "../etc"), offMount},
		{"a sibling with a shared stem", filesBackup([]b2backupv1.VolumeSpec{data}, nil, "/database"), offMount},
		{"two claims on one path", filesBackup([]b2backupv1.VolumeSpec{data, {ExistingClaim: "b", MountPath: "/data/"}},
			nil, dataMount), "both mounted at /data"},
		{"scratch on a claim's path", filesBackup([]b2backupv1.VolumeSpec{data}, &b2backupv1.ScratchSpec{MountPath: dataMount},
			dataMount), "both mounted"},
		{"a claim over the database Secret", filesBackup([]b2backupv1.VolumeSpec{
			{ExistingClaim: "x", MountPath: b2backupv1.DBSecretMountPath}}, nil, b2backupv1.DBSecretMountPath),
			"database Secret"},
		{"a claim at the root", filesBackup([]b2backupv1.VolumeSpec{{ExistingClaim: "x", MountPath: "/"}}, nil, "/"),
			"cannot be mounted at /"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tt.sb)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestEveryVolumeAndTheScratchSpaceAreMounted(t *testing.T) {
	t.Parallel()
	limit := resource.MustParse("2Gi")
	sb := filesBackup([]b2backupv1.VolumeSpec{
		{ExistingClaim: "wx", MountPath: dataMount, ReadOnly: true}, {ExistingClaim: "lxmd"},
	}, &b2backupv1.ScratchSpec{MountPath: scratchMount, SizeLimit: &limit}, dataMount)
	sb.Spec.WorkingDir = scratchMount
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	c := pod.Containers[0]
	if c.WorkingDir != scratchMount {
		t.Errorf("workingDir = %q, want /backup", c.WorkingDir)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}
	tests := []struct {
		path, volume, claim string
		readOnly            bool
	}{
		{dataMount, "data-0", "wx", true},
		{"/lxmd", "data-1", "lxmd", false},
	}
	for _, tt := range tests {
		m, ok := mounts[tt.path]
		if !ok || m.Name != tt.volume || m.ReadOnly != tt.readOnly {
			t.Errorf("mount at %s = %+v", tt.path, m)
		}
		v := volumes[tt.volume]
		if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != tt.claim ||
			v.PersistentVolumeClaim.ReadOnly != tt.readOnly {
			t.Errorf("volume %s = %+v", tt.volume, v)
		}
	}
	if m := mounts[scratchMount]; m.Name != ScratchVolume {
		t.Errorf("scratch mount = %+v", m)
	}
	if v := volumes[ScratchVolume]; v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil || !v.EmptyDir.SizeLimit.Equal(limit) {
		t.Errorf("scratch volume = %+v", v)
	}
	aff := pod.Affinity
	if aff == nil || aff.PodAffinity == nil || len(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Errorf("affinity = %+v, want one required rule beside the app", aff)
	}
}

func TestWorkingDirDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sb   *b2backupv1.ScheduledBackup
		want string
	}{
		{"the first volume", filesBackup([]b2backupv1.VolumeSpec{{ExistingClaim: "a"}, {ExistingClaim: "b"}}, nil), "/a"},
		{"the scratch space", filesBackup(nil, &b2backupv1.ScratchSpec{MountPath: "/backup/"}), scratchMount},
		{"the root", testBackup(nil), "/"},
		{"an explicit one", testBackup(func(sb *b2backupv1.ScheduledBackup) { sb.Spec.WorkingDir = "/srv" }), "/srv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containerOf(t, tt.sb, testConfig()).WorkingDir; got != tt.want {
				t.Errorf("workingDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMountFor(t *testing.T) {
	t.Parallel()
	sb := filesBackup([]b2backupv1.VolumeSpec{
		{ExistingClaim: configClaim, MountPath: dataMount}, {ExistingClaim: "media", MountPath: "/data/media"},
	}, &b2backupv1.ScratchSpec{MountPath: "/data/tmp"})
	tests := []struct {
		path string
		want string
	}{
		{dataMount, configClaim},
		{"/data/app", configClaim},
		{"/data/media/x", "media"},
		{"/data/mediax", configClaim},
		{"/data/tmp/x", ""},
		{"/elsewhere", ""},
	}
	for _, tt := range tests {
		got := ""
		if v := MountFor(sb, tt.path); v != nil {
			got = v.ExistingClaim
		}
		if got != tt.want {
			t.Errorf("MountFor(%s) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestValidateRefusesWhatForgetWouldMerge(t *testing.T) {
	t.Parallel()
	retention := &b2backupv1.Retention{Daily: new(int32(7))}
	tests := []struct {
		name    string
		mutate  func(*b2backupv1.ScheduledBackup)
		wantErr string
	}{
		{"two databases under the default tag", func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Sources = []b2backupv1.BackupSource{
				{Type: b2backupv1.SourceTypeCNPG, Database: appDir}, {Type: b2backupv1.SourceTypeCNPG, Database: "keycloak"},
			}
		}, "both tagged \"db\""},
		{"two files sources under one tag", func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Volumes = []b2backupv1.VolumeSpec{{ExistingClaim: dataClaim, MountPath: dataMount}}
			sb.Spec.Sources = []b2backupv1.BackupSource{
				{Type: b2backupv1.SourceTypeFiles, Paths: []string{"/data/a"}},
				{Type: b2backupv1.SourceTypeFiles, Paths: []string{"/data/b"}},
			}
		}, "both tagged \"files\""},
		{"retention beside a script", func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Sources = nil
			sb.Spec.Script = "restic backup /data"
		}, "not applied to spec.script"},
		{"distinct tags", func(sb *b2backupv1.ScheduledBackup) {
			sb.Spec.Sources = []b2backupv1.BackupSource{
				{Type: b2backupv1.SourceTypeCNPG, Database: appDir},
				{Type: b2backupv1.SourceTypeCNPG, Database: "keycloak", Tag: "db-keycloak"},
			}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sb := testBackup(func(sb *b2backupv1.ScheduledBackup) {
				sb.Spec.Retention = retention
				tt.mutate(sb)
			})
			_, err := BuildJobTemplate(sb, testRepo(), testConfig())
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("BuildJobTemplate() = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("BuildJobTemplate() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestReadsEveryFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		pod       *corev1.PodSecurityContext
		container *corev1.SecurityContext
		want      bool
	}{
		{"the runtime default", nil, nil, true},
		{"the operator's default", nil, &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{dropAll}, Add: []corev1.Capability{BackupCapability}}}, true},
		{"every capability dropped", nil, &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{dropAll}}}, false},
		{"DAC_READ_SEARCH kept", nil, &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{dropAll}, Add: []corev1.Capability{"DAC_READ_SEARCH"}}}, true},
		{"a non-root user", &corev1.PodSecurityContext{RunAsUser: new(int64(1000))}, nil, false},
		{"root named in the container", &corev1.PodSecurityContext{RunAsUser: new(int64(1000))},
			&corev1.SecurityContext{RunAsUser: new(int64(0))}, true},
	}
	for _, tt := range tests {
		if got := ReadsEveryFile(tt.pod, tt.container); got != tt.want {
			t.Errorf("%s: ReadsEveryFile = %t, want %t", tt.name, got, tt.want)
		}
	}
}
