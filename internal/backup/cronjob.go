// Package backup builds the CronJobs and Jobs that run restic.
package backup

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	cacheMountPath = "/cache"

	// JobTTLSeconds is how long a finished backup Job is kept.
	JobTTLSeconds int32 = 3600

	hostnameTopologyKey = "kubernetes.io/hostname"

	preferredAffinityWeight int32 = 100

	labelName       = "app.kubernetes.io/name"
	labelController = "app.kubernetes.io/controller"
	labelManagedBy  = "app.kubernetes.io/managed-by"
	managedByValue  = "b2-backup-operator"

	containerName = "restic"
	cacheVolume   = "cache"
	mariadbName   = "mariadb"

	envTZ               = "TZ"
	envResticHost       = "RESTIC_HOST"
	envResticRepository = "RESTIC_REPOSITORY"
	envResticPassword   = "RESTIC_PASSWORD"
	envAccessKeyID      = "AWS_ACCESS_KEY_ID"
	envSecretAccessKey  = "AWS_SECRET_ACCESS_KEY"
	envDefaultRegion    = "AWS_DEFAULT_REGION"
	envDBHost           = "DB_HOST"
	envDBDatabase       = "DB_DATABASE"
	envDBUsername       = "DB_USERNAME"
)

// EnvProgressFPS makes restic print a status line to a log, which has no
// terminal, at that many lines a second. b2bk's dashboard reads those lines.
// The operator sets it to DefaultProgressFPS unless spec.env sets it.
const (
	EnvProgressFPS     = "RESTIC_PROGRESS_FPS"
	DefaultProgressFPS = "0.1"
)

const fromRepositorySecret = "the Repository's Secret"

var envSource = map[string]string{
	envTZ:               "spec.timeZone",
	envResticHost:       "spec.resticHost",
	envResticRepository: fromRepositorySecret,
	envResticPassword:   fromRepositorySecret,
	envAccessKeyID:      fromRepositorySecret,
	envSecretAccessKey:  fromRepositorySecret,
	envDefaultRegion:    fromRepositorySecret,
	envDBHost:           "spec.database.host",
	envDBDatabase:       "spec.database.name",
	envDBUsername:       "spec.database.user",
}

// Config is the operator-wide backup configuration.
type Config struct {
	Image string

	CacheStorageClass string
}

// DefaultImageRepository is the repository of the backup image the operator runs by default.
const DefaultImageRepository = "ghcr.io/usa-reddragon/restic"

// DefaultImage is the backup image the operator runs by default.
const DefaultImage = "ghcr.io/usa-reddragon/restic:0.19.1@sha256:46ede49bbb4be096724774839836595bc7f82e90dcf9ae1945b45ab3c8a7d51e"

// DefaultConfig returns the configuration the operator runs with when no flags are set.
func DefaultConfig() Config {
	return Config{
		Image: DefaultImage,
	}
}

const (
	// LabelTrigger marks how a backup Job was started.
	LabelTrigger = "b2backup.mcswain.dev/trigger"
	// TriggerManual is the LabelTrigger value for a triggered backup.
	TriggerManual = "manual"
)

const (
	// DataVolumePrefix starts the pod volume name of every claim in spec.volumes.
	DataVolumePrefix = "data-"
	// ScratchVolume is the pod volume name of spec.scratch.
	ScratchVolume = "scratch"
)

// DataVolumeName returns the pod volume name of spec.volumes[i].
func DataVolumeName(i int) string { return DataVolumePrefix + strconv.Itoa(i) }

// MountFor returns the claim in spec.volumes holding p, the longest mount path
// that contains it, or nil when p is on scratch space or on no volume at all.
func MountFor(sb *b2backupv1.ScheduledBackup, p string) *b2backupv1.VolumeSpec {
	var best *b2backupv1.VolumeSpec
	bestLen := -1
	for i := range sb.Spec.Volumes {
		v := &sb.Spec.Volumes[i]
		if m := v.EffectiveMountPath(); under(p, m) && len(m) > bestLen {
			best, bestLen = v, len(m)
		}
	}
	if sc := sb.Spec.Scratch; sc != nil {
		if m := path.Clean(sc.MountPath); under(p, m) && len(m) > bestLen {
			return nil
		}
	}
	return best
}

func under(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// Validate checks what the CRD cannot: that no two sources share a tag, that no
// two mounts share a path, that none is a path the operator mounts itself, and
// that every files source path lies on a volume or the scratch space.
//
// Two sources under one tag would fall into one forget group, since the image
// groups by tags, and the retention policy would keep one snapshot per period
// across both, expiring the other source's.
func Validate(sb *b2backupv1.ScheduledBackup) error {
	if sb.Spec.Script != "" && sb.Spec.Retention != nil {
		return fmt.Errorf("spec.retention is not applied to spec.script; forget in the script itself, with --tag")
	}
	tags := map[string]int{}
	for i, src := range sb.Spec.Sources {
		if j, taken := tags[src.EffectiveTag()]; taken {
			return fmt.Errorf("sources %d and %d are both tagged %q; give each source a tag of its own, "+
				"or forget would expire one source's snapshots to keep the other's", j, i, src.EffectiveTag())
		}
		tags[src.EffectiveTag()] = i
	}

	seen := map[string]string{
		cacheMountPath:               "the restic cache",
		b2backupv1.DBSecretMountPath: "the database Secret",
	}
	claim := func(p, what string) error {
		p = path.Clean(p)
		if p == "/" {
			return fmt.Errorf("%s cannot be mounted at /", what)
		}
		if other, taken := seen[p]; taken {
			return fmt.Errorf("%s and %s are both mounted at %s", what, other, p)
		}
		seen[p] = what
		return nil
	}
	for i := range sb.Spec.Volumes {
		v := &sb.Spec.Volumes[i]
		if err := claim(v.EffectiveMountPath(), "claim "+v.ExistingClaim); err != nil {
			return err
		}
	}
	if sc := sb.Spec.Scratch; sc != nil {
		if err := claim(sc.MountPath, "the scratch space"); err != nil {
			return err
		}
	}

	workingDir := sb.EffectiveWorkingDir()
	for _, src := range sb.Spec.Sources {
		if src.Type != b2backupv1.SourceTypeFiles {
			continue
		}
		for _, p := range src.ResolvedPaths(workingDir) {
			if !onMount(sb, p) {
				return fmt.Errorf("files source %q backs up %s, which is on none of spec.volumes or spec.scratch",
					src.EffectiveTag(), p)
			}
		}
	}
	return nil
}

func onMount(sb *b2backupv1.ScheduledBackup, p string) bool {
	for i := range sb.Spec.Volumes {
		if under(p, sb.Spec.Volumes[i].EffectiveMountPath()) {
			return true
		}
	}
	return sb.Spec.Scratch != nil && under(p, path.Clean(sb.Spec.Scratch.MountPath))
}

// CacheName returns the name of the restic cache PVC.
func CacheName(sb *b2backupv1.ScheduledBackup) string { return sb.Name + "-cache" }

// CronJobSuffix ends the name of every backup CronJob.
const CronJobSuffix = "-backup"

// CronJobName returns the name of the backup CronJob.
func CronJobName(sb *b2backupv1.ScheduledBackup) string { return sb.Name + CronJobSuffix }

const maxManualJobName = 52

// ManualJobName returns the Job name for a backup triggered at the given time.
func ManualJobName(sb *b2backupv1.ScheduledBackup, at time.Time) string {
	return manualJobName(sb.Name, "-manual-"+strconv.FormatInt(at.Unix(), 36))
}

func manualJobName(name, suffix string) string {
	if limit := max(0, maxManualJobName-len(suffix)); len(name) > limit {
		name = name[:limit]
	}
	return name + suffix
}

// BuildManualJob builds a one-off backup Job.
func BuildManualJob(
	sb *b2backupv1.ScheduledBackup,
	repo *b2backupv1.Repository,
	cfg Config,
	at time.Time,
) (*batchv1.Job, error) {
	tmpl, err := BuildJobTemplate(sb, repo, cfg)
	if err != nil {
		return nil, err
	}

	labels := maps.Clone(tmpl.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelTrigger] = TriggerManual

	return &batchv1.Job{
		Name:        ManualJobName(sb, at),
		Namespace:   sb.Namespace,
		Labels:      labels,
		Annotations: maps.Clone(tmpl.Annotations),
		Spec:        tmpl.Spec,
	}, nil
}

func cacheEnabled(sb *b2backupv1.ScheduledBackup) bool {
	if sb.Spec.Cache == nil || sb.Spec.Cache.Enabled == nil {
		return true
	}
	return *sb.Spec.Cache.Enabled
}

// BuildCachePVC builds the restic cache claim, or nil when caching is off.
func BuildCachePVC(sb *b2backupv1.ScheduledBackup, cfg Config) (*corev1.PersistentVolumeClaim, error) {
	if !cacheEnabled(sb) {
		return nil, nil
	}

	size, accessMode, storageClass := "1Gi", corev1.ReadWriteOnce, cfg.CacheStorageClass
	if c := sb.Spec.Cache; c != nil {
		if c.Size != "" {
			size = c.Size
		}
		if c.AccessMode != "" {
			accessMode = c.AccessMode
		}
		if c.StorageClass != "" {
			storageClass = c.StorageClass
		}
	}

	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("parsing cache size %q: %w", size, err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		Name:      CacheName(sb),
		Namespace: sb.Namespace,
		Labels:    commonLabels(sb),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	if storageClass != "" {
		pvc.Spec.StorageClassName = ptr.To(storageClass)
	}
	return pvc, nil
}

// CredentialsSecret is the Secret a Job reads the repository's credentials
// from: the one the Repository recorded writing, or the one its spec names
// before it has recorded any.
func CredentialsSecret(repo *b2backupv1.Repository) string {
	if repo.Status.SecretName != "" {
		return repo.Status.SecretName
	}
	return repo.SecretName()
}

func commonLabels(sb *b2backupv1.ScheduledBackup) map[string]string {
	return map[string]string{
		labelName:      sb.Name,
		labelManagedBy: managedByValue,
	}
}

// BuildJobTemplate builds the pod template shared by scheduled and manual backups.
func BuildJobTemplate(
	sb *b2backupv1.ScheduledBackup,
	repo *b2backupv1.Repository,
	cfg Config,
) (batchv1.JobTemplateSpec, error) {
	if err := Validate(sb); err != nil {
		return batchv1.JobTemplateSpec{}, err
	}
	script, err := Render(sb)
	if err != nil {
		return batchv1.JobTemplateSpec{}, err
	}

	image := sb.Spec.Image
	if image == "" {
		image = cfg.Image
	}
	if image == "" {
		return batchv1.JobTemplateSpec{}, fmt.Errorf("no backup image configured")
	}

	env, err := buildEnv(sb)
	if err != nil {
		return batchv1.JobTemplateSpec{}, err
	}
	volumes, mounts := buildVolumes(sb)

	container := corev1.Container{
		Name:            containerName,
		Image:           image,
		Command:         []string{"sh", "-c", script},
		Env:             env,
		EnvFrom:         []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{Name: CredentialsSecret(repo)}}},
		VolumeMounts:    mounts,
		WorkingDir:      sb.EffectiveWorkingDir(),
		Resources:       resources(sb),
		SecurityContext: containerSecurityContext(sb),
	}

	return batchv1.JobTemplateSpec{
		Labels:      commonLabels(sb),
		Annotations: map[string]string{AnnotationForgetScope: ScopeOf(sb).encode()},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To(JobTTLSeconds),

			BackoffLimit: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				Labels: podLabels(sb),
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{container},
					Volumes:       volumes,
					Affinity:      buildAffinity(sb),
					NodeSelector:  sb.Spec.NodeSelector,
					Tolerations:   sb.Spec.Tolerations,

					PriorityClassName: sb.Spec.PriorityClassName,

					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext:              podSecurityContext(sb),
				},
			},
		},
	}, nil
}

// BuildCronJob builds the CronJob that runs scheduled backups.
func BuildCronJob(
	sb *b2backupv1.ScheduledBackup,
	repo *b2backupv1.Repository,
	cfg Config,
) (*batchv1.CronJob, error) {
	schedule, err := ResolveScheduleIn(sb.Spec.Schedule, sb.Namespace+"/"+sb.Name, sb.Spec.TimeZone)
	if err != nil {
		return nil, err
	}

	jobTemplate, err := BuildJobTemplate(sb, repo, cfg)
	if err != nil {
		return nil, err
	}

	return &batchv1.CronJob{
		Name:      CronJobName(sb),
		Namespace: sb.Namespace,
		Labels:    commonLabels(sb),
		Spec: batchv1.CronJobSpec{
			Schedule:                   schedule,
			TimeZone:                   timeZone(sb),
			ConcurrencyPolicy:          concurrencyPolicy(sb),
			Suspend:                    ptr.To(sb.Spec.Suspend || RestoreHold(sb) != ""),
			SuccessfulJobsHistoryLimit: sb.Spec.SuccessfulJobsHistoryLimit,
			FailedJobsHistoryLimit:     sb.Spec.FailedJobsHistoryLimit,
			JobTemplate:                jobTemplate,
		},
	}, nil
}

func timeZone(sb *b2backupv1.ScheduledBackup) *string {
	if sb.Spec.TimeZone == "" {
		return nil
	}
	return ptr.To(sb.Spec.TimeZone)
}

func concurrencyPolicy(sb *b2backupv1.ScheduledBackup) batchv1.ConcurrencyPolicy {
	if sb.Spec.ConcurrencyPolicy != "" {
		return sb.Spec.ConcurrencyPolicy
	}
	return batchv1.ForbidConcurrent
}

func resources(sb *b2backupv1.ScheduledBackup) corev1.ResourceRequirements {
	if sb.Spec.Resources != nil {
		return *sb.Spec.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

func podLabels(sb *b2backupv1.ScheduledBackup) map[string]string {
	labels := map[string]string{}
	if sb.Spec.Database != nil && sb.Spec.Database.Engine == b2backupv1.DatabaseEngineMariaDB {
		labels["mariadb-client"] = "true"
	}
	maps.Copy(labels, sb.Spec.PodLabels)
	maps.Copy(labels, commonLabels(sb))
	return labels
}

// podSecurityContext returns the pod's security context, which by default sets
// only the seccomp profile.
//
// Pinning a user or group id is what makes a backup pod hard to run: it has to
// read the app's data, whose ownership varies per app and per cluster, and an
// fsGroup would chown that data as a side effect of backing it up. A namespace
// needing more supplies it through spec.podSecurityContext.
func podSecurityContext(sb *b2backupv1.ScheduledBackup) *corev1.PodSecurityContext {
	if sb.Spec.PodSecurityContext != nil {
		return sb.Spec.PodSecurityContext
	}
	return &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func containerSecurityContext(sb *b2backupv1.ScheduledBackup) *corev1.SecurityContext {
	if sb.Spec.ContainerSecurityContext != nil {
		return sb.Spec.ContainerSecurityContext
	}
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{BackupCapability},
		},
	}
}

// ReadsEveryFile reports whether a container running with these security contexts
// can read every file on a claim, whatever its owner and mode: it runs as root
// and keeps DAC_OVERRIDE or DAC_READ_SEARCH. A nil container context is the
// runtime's default capability set, which includes DAC_OVERRIDE.
func ReadsEveryFile(pod *corev1.PodSecurityContext, container *corev1.SecurityContext) bool {
	uid := int64(0)
	if pod != nil && pod.RunAsUser != nil {
		uid = *pod.RunAsUser
	}
	if container != nil && container.RunAsUser != nil {
		uid = *container.RunAsUser
	}
	if uid != 0 {
		return false
	}
	if container == nil || container.Capabilities == nil {
		return true
	}
	caps := container.Capabilities
	for _, c := range []corev1.Capability{"DAC_OVERRIDE", "DAC_READ_SEARCH"} {
		if slices.Contains(caps.Add, c) {
			return true
		}
	}
	return !slices.Contains(caps.Drop, "ALL") && !slices.Contains(caps.Drop, "DAC_OVERRIDE")
}

// EffectiveSecurityContexts returns the pod and container security contexts a
// backup runs with.
func EffectiveSecurityContexts(sb *b2backupv1.ScheduledBackup) (*corev1.PodSecurityContext, *corev1.SecurityContext) {
	return podSecurityContext(sb), containerSecurityContext(sb)
}

// BackupCapability lets the backup, which runs as root, read every file on the
// claims it mounts, as the app's own runtime-default container could. Without
// it a directory another user owns with mode 0700 is left out of the snapshot.
// DAC_READ_SEARCH would do, but the baseline Pod Security Standard does not
// allow it; DAC_OVERRIDE it does.
const BackupCapability corev1.Capability = "DAC_OVERRIDE"

func buildEnv(sb *b2backupv1.ScheduledBackup) ([]corev1.EnvVar, error) {
	env := []corev1.EnvVar{
		{Name: envTZ, Value: sb.Spec.TimeZone},
		resticHostEnv(sb),
	}

	if db := sb.Spec.Database; db != nil && db.Engine == b2backupv1.DatabaseEngineMariaDB {
		if db.Host == "" || db.Name == "" || db.User == "" {
			return nil, fmt.Errorf("database.host, database.name and database.user are required for the mariadb engine")
		}
		env = append(env,
			corev1.EnvVar{Name: envDBHost, Value: db.Host},
			corev1.EnvVar{Name: envDBDatabase, Value: db.Name},
			corev1.EnvVar{Name: envDBUsername, Value: db.User},
		)
	}

	if err := rejectReservedEnv(sb.Spec.Env, reservedEnvNames(env)); err != nil {
		return nil, err
	}

	if _, set := sb.Spec.Env[EnvProgressFPS]; !set {
		env = append(env, corev1.EnvVar{Name: EnvProgressFPS, Value: DefaultProgressFPS})
	}
	for _, k := range slices.Sorted(maps.Keys(sb.Spec.Env)) {
		env = append(env, corev1.EnvVar{Name: k, Value: sb.Spec.Env[k]})
	}
	return env, nil
}

func resticHostEnv(sb *b2backupv1.ScheduledBackup) corev1.EnvVar {
	if sb.Spec.ResticHost != "" {
		return corev1.EnvVar{Name: envResticHost, Value: sb.Spec.ResticHost}
	}
	return corev1.EnvVar{
		Name: envResticHost,

		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		},
	}
}

func reservedEnvNames(operatorSet []corev1.EnvVar) map[string]struct{} {
	fromSecret := []string{
		envResticRepository, envResticPassword, envAccessKeyID, envSecretAccessKey, envDefaultRegion,
	}
	reserved := make(map[string]struct{}, len(operatorSet)+len(fromSecret))
	for _, e := range operatorSet {
		reserved[e.Name] = struct{}{}
	}
	for _, name := range fromSecret {
		reserved[name] = struct{}{}
	}
	return reserved
}

func rejectReservedEnv(user map[string]string, reserved map[string]struct{}) error {
	if len(user) == 0 {
		return nil
	}

	var conflicts []string
	for _, name := range slices.Sorted(maps.Keys(user)) {
		if _, taken := reserved[name]; !taken {
			continue
		}
		if source, known := envSource[name]; known {
			name += " (set from " + source + ")"
		}
		conflicts = append(conflicts, name)
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("spec.env must not set %s: a container's env overrides its envFrom, "+
		"so these would silently change where or how this backup is stored",
		strings.Join(conflicts, ", "))
}

func buildVolumes(sb *b2backupv1.ScheduledBackup) ([]corev1.Volume, []corev1.VolumeMount) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	if cacheEnabled(sb) {
		volumes = append(volumes, corev1.Volume{
			Name:                  cacheVolume,
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: CacheName(sb)},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: cacheVolume, MountPath: cacheMountPath})
	}

	if db := sb.Spec.Database; db != nil {
		volumes = append(volumes, corev1.Volume{
			Name:   "db-credentials",
			Secret: &corev1.SecretVolumeSource{SecretName: db.EffectiveSecretName()},
		})

		mounts = append(mounts, corev1.VolumeMount{
			Name: "db-credentials", MountPath: b2backupv1.DBSecretMountPath, ReadOnly: true,
		})
	}

	for i := range sb.Spec.Volumes {
		v := &sb.Spec.Volumes[i]
		name := DataVolumeName(i)
		volumes = append(volumes, corev1.Volume{
			Name: name,
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: v.ExistingClaim,
				ReadOnly:  v.ReadOnly,
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name: name, MountPath: v.EffectiveMountPath(), ReadOnly: v.ReadOnly,
		})
	}

	if sc := sb.Spec.Scratch; sc != nil {
		volumes = append(volumes, corev1.Volume{
			Name:     ScratchVolume,
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: sc.SizeLimit},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: ScratchVolume, MountPath: path.Clean(sc.MountPath)})
	}

	return volumes, mounts
}

func buildAffinity(sb *b2backupv1.ScheduledBackup) *corev1.Affinity {
	if sb.Spec.Affinity != nil {
		return sb.Spec.Affinity
	}

	if len(sb.Spec.Volumes) > 0 {
		return appAffinity(sb.Namespace)
	}

	db := sb.Spec.Database
	if db == nil {
		return nil
	}

	switch db.Engine {
	case b2backupv1.DatabaseEngineMariaDB:
		return podAffinity(db.RequireSameNode, map[string]string{
			labelName:       mariadbName,
			labelController: mariadbName,
		})
	case b2backupv1.DatabaseEngineCNPG:
		return podAffinity(db.RequireSameNode, map[string]string{
			"cnpg.io/cluster":      db.EffectiveCNPGCluster(),
			"cnpg.io/instanceRole": "primary",
		})
	}
	return nil
}

func appAffinity(name string) *corev1.Affinity {
	return &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: hostnameTopologyKey,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{labelName: name},
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      labelManagedBy,
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{managedByValue},
					}},
				},
			}},
		},
	}
}

func podAffinity(required bool, labels map[string]string) *corev1.Affinity {
	term := corev1.PodAffinityTerm{
		TopologyKey:   hostnameTopologyKey,
		LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
	}
	if required {
		return &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{term},
			},
		}
	}
	return &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight:          preferredAffinityWeight,
				PodAffinityTerm: term,
			}},
		},
	}
}
