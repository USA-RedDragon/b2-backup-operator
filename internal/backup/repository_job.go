package backup

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// RepositoryJobSpec is the Job a one-off restic command runs in against a
// Repository that no ScheduledBackup uses: the repository's credentials, an
// emptyDir for the restic cache, and no data, database or scheduling of any
// backup's.
func RepositoryJobSpec(repo *b2backupv1.Repository, image string) batchv1.JobSpec {
	if image == "" {
		image = DefaultImage
	}
	container := corev1.Container{
		Name:  containerName,
		Image: image,
		Env: []corev1.EnvVar{{
			Name:      envResticHost,
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
		}},
		EnvFrom:      []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{Name: CredentialsSecret(repo)}}},
		VolumeMounts: []corev1.VolumeMount{{Name: cacheVolume, MountPath: cacheMountPath}},
		Resources:    resources(&b2backupv1.ScheduledBackup{}),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	return batchv1.JobSpec{
		TTLSecondsAfterFinished: ptr.To(JobTTLSeconds),
		BackoffLimit:            ptr.To(int32(0)),
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{container},
				Volumes: []corev1.Volume{{
					Name: cacheVolume, EmptyDir: &corev1.EmptyDirVolumeSource{},
				}},
				AutomountServiceAccountToken: ptr.To(false),
				SecurityContext: &corev1.PodSecurityContext{
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			},
		},
	}
}
