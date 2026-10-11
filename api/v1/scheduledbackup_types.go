package v1

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SourceType is the kind of data a BackupSource backs up.
// +kubebuilder:validation:Enum=cnpg;mariadb;files
type SourceType string

const (
	// SourceTypeCNPG dumps a CloudNativePG database.
	SourceTypeCNPG SourceType = "cnpg"

	// SourceTypeMariaDB dumps a MariaDB database.
	SourceTypeMariaDB SourceType = "mariadb"

	// SourceTypeFiles backs up a path on the mounted volume.
	SourceTypeFiles SourceType = "files"
)

// DatabaseEngine is the database a DatabaseSpec connects to.
// +kubebuilder:validation:Enum=cnpg;mariadb
type DatabaseEngine string

const (
	// DatabaseEngineCNPG is CloudNativePG.
	DatabaseEngineCNPG DatabaseEngine = "cnpg"

	// DatabaseEngineMariaDB is MariaDB.
	DatabaseEngineMariaDB DatabaseEngine = "mariadb"
)

// DBSecretMountPath is where the database credentials Secret is mounted in the backup container.
const DBSecretMountPath = "/var/run/secrets/b2backup/database"

const (
	defaultCNPGSecretName    = "postgresql-app"
	defaultMariaDBSecretName = "mariadb"
	defaultCNPGCluster       = "postgresql"
)

const (
	// ScheduledBackupConditionReady reports whether the backup is scheduled and runnable.
	ScheduledBackupConditionReady = "Ready"
)

// BackupSource is one thing to back up on each run.
// +kubebuilder:validation:XValidation:rule="self.type == 'files' ? (!has(self.database) && !has(self.extraArgs)) : (!has(self.path) && !has(self.exclude))",message="path and exclude are only valid for files sources; database and extraArgs only for database sources"
type BackupSource struct {
	// Type is the kind of data this source backs up.
	// +required
	Type SourceType `json:"type"`

	// Tag is the restic tag for this source's snapshots. Defaults to "files" or "db".
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+$`
	// +optional
	Tag string `json:"tag,omitempty"`

	// Path is the directory to back up, relative to the volume mount. Files sources only.
	// +optional
	Path string `json:"path,omitempty"`

	// Exclude is a list of restic exclude patterns. Files sources only.
	// +optional
	Exclude []string `json:"exclude,omitempty"`

	// Database is the database name to dump. Database sources only.
	// +optional
	Database string `json:"database,omitempty"`

	// ExtraArgs is passed to the dump command. Database sources only.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// EffectiveTag returns the restic tag to use for this source.
func (s BackupSource) EffectiveTag() string {
	if s.Tag != "" {
		return s.Tag
	}
	if s.Type == SourceTypeFiles {
		return "files"
	}
	return "db"
}

// EffectivePath returns the path to back up.
func (s BackupSource) EffectivePath() string {
	if s.Path != "" {
		return s.Path
	}
	return "."
}

// Retention is how many snapshots restic forget keeps.
// +kubebuilder:validation:XValidation:rule="has(self.last) || has(self.hourly) || has(self.daily) || has(self.weekly) || has(self.monthly) || has(self.yearly)",message="at least one retention field must be set; omit retention entirely to skip forgetting"
type Retention struct {
	// Last keeps the n most recent snapshots.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Last *int32 `json:"last,omitempty"`

	// Hourly keeps the last snapshot of each of the n most recent hours.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Hourly *int32 `json:"hourly,omitempty"`

	// Daily keeps the last snapshot of each of the n most recent days.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Daily *int32 `json:"daily,omitempty"`

	// Weekly keeps the last snapshot of each of the n most recent weeks.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Weekly *int32 `json:"weekly,omitempty"`

	// Monthly keeps the last snapshot of each of the n most recent months.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Monthly *int32 `json:"monthly,omitempty"`

	// Yearly keeps the last snapshot of each of the n most recent years.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Yearly *int32 `json:"yearly,omitempty"`
}

// DatabaseSpec is the database the backup container connects to.
// +kubebuilder:validation:XValidation:rule="self.engine != 'mariadb' || (has(self.host) && has(self.name) && has(self.user))",message="host, name and user are required for the mariadb engine"
type DatabaseSpec struct {
	// Engine is the database to connect to.
	// +required
	Engine DatabaseEngine `json:"engine"`

	// SecretName holds the credentials. Defaults to "postgresql-app" for cnpg and "mariadb" for mariadb.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Host is the database host. Required for mariadb.
	// +optional
	Host string `json:"host,omitempty"`

	// Name is the default database. Required for mariadb.
	// +optional
	Name string `json:"name,omitempty"`

	// User is the database user. Required for mariadb.
	// +optional
	User string `json:"user,omitempty"`

	// CNPGCluster is the CloudNativePG cluster to schedule beside. Defaults to "postgresql". cnpg only.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`
	// +optional
	CNPGCluster string `json:"cnpgCluster,omitempty"`

	// RequireSameNode makes the affinity to the database pod a hard requirement.
	// Without it the backup pod prefers that node but will run anywhere.
	// +optional
	RequireSameNode bool `json:"requireSameNode,omitempty"`
}

// EffectiveSecretName returns the Secret holding the database credentials.
func (d *DatabaseSpec) EffectiveSecretName() string {
	if d.SecretName != "" {
		return d.SecretName
	}
	if d.Engine == DatabaseEngineMariaDB {
		return defaultMariaDBSecretName
	}
	return defaultCNPGSecretName
}

// EffectiveCNPGCluster returns the CloudNativePG cluster to schedule beside.
func (d *DatabaseSpec) EffectiveCNPGCluster() string {
	if d.CNPGCluster != "" {
		return d.CNPGCluster
	}
	return defaultCNPGCluster
}

// VolumeSpec is an existing PVC to mount into the backup container.
type VolumeSpec struct {
	// ExistingClaim is the name of the PersistentVolumeClaim to mount.
	// +required
	ExistingClaim string `json:"existingClaim"`

	// MountPath is where to mount the claim. Defaults to /<existingClaim>.
	// +kubebuilder:validation:XValidation:rule="self != '/cache'",message="mountPath must not be /cache, which is where the restic cache volume is mounted"
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// ReadOnly mounts the claim read-only.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// EffectiveMountPath returns where the volume is mounted.
func (v *VolumeSpec) EffectiveMountPath() string {
	if v.MountPath != "" {
		return v.MountPath
	}
	return "/" + v.ExistingClaim
}

// CacheSpec is the PVC backing restic's cache.
type CacheSpec struct {
	// Enabled turns the cache volume on. Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// StorageClass for the cache claim. Defaults to the operator's setting.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// Size of the cache claim.
	// +kubebuilder:default:="1Gi"
	// +optional
	Size string `json:"size,omitempty"`

	// AccessMode for the cache claim.
	// +kubebuilder:default:="ReadWriteMany"
	// +optional
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
}

// ScheduledBackupSpec defines the desired state of a ScheduledBackup.
// +kubebuilder:validation:XValidation:rule="!has(self.env) || self.env.all(k, !(k in ['RESTIC_REPOSITORY', 'RESTIC_PASSWORD', 'AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_DEFAULT_REGION']))",message="spec.env must not set RESTIC_REPOSITORY, RESTIC_PASSWORD, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY or AWS_DEFAULT_REGION: they come from the Repository's Secret, and a container's env overrides envFrom"
type ScheduledBackupSpec struct {
	// RepositoryRef is the Repository in this namespace to back up into.
	// +required
	RepositoryRef corev1.LocalObjectReference `json:"repositoryRef"`

	// Schedule is a cron expression, or a shorthand such as @daily or @every 6h.
	// +required
	Schedule string `json:"schedule"`

	// TimeZone the schedule is interpreted in.
	// +kubebuilder:default:="America/Chicago"
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// Suspend stops new backups without deleting the resource.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// ConcurrencyPolicy decides what happens when a run overlaps the previous one.
	// +kubebuilder:validation:Enum=Allow;Forbid;Replace
	// +kubebuilder:default:=Forbid
	// +optional
	ConcurrencyPolicy batchv1.ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// Image overrides the backup container image.
	// +optional
	Image string `json:"image,omitempty"`

	// Sources is what to back up. Mutually exclusive with script.
	// +kubebuilder:validation:MinItems=1
	// +optional
	Sources []BackupSource `json:"sources,omitempty"`

	// Script is a shell script to run instead of sources. Mutually exclusive with sources.
	// +optional
	Script string `json:"script,omitempty"`

	// Retention is how many snapshots to keep. Omit to never forget.
	// +optional
	Retention *Retention `json:"retention,omitempty"`

	// RetryLock is how long restic waits for a lock held by another run.
	// +kubebuilder:default:="5m"
	// +kubebuilder:validation:XValidation:rule="self == '0' || self.matches('^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h))+$')",message="must be a Go duration such as 30m, 1h or 1h30m"
	// +optional
	RetryLock *metav1.Duration `json:"retryLock,omitempty"`

	// Env is extra environment variables for the backup container. Names the operator
	// sets itself are rejected, because a container's env overrides its envFrom.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// ResticHost is the host recorded on every snapshot, and the base name of the
	// database dump inside one. Defaults to the namespace. Set it to keep snapshots
	// in one series across a namespace rename.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$`
	// +optional
	ResticHost string `json:"resticHost,omitempty"`

	// Database is the database to connect to. Required by database sources.
	// +optional
	Database *DatabaseSpec `json:"database,omitempty"`

	// Volume is the PVC to mount. Required by files sources.
	// +optional
	Volume *VolumeSpec `json:"volume,omitempty"`

	// Cache configures restic's cache volume.
	// +optional
	Cache *CacheSpec `json:"cache,omitempty"`

	// Affinity for the backup pod.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// PodSecurityContext for the backup pod.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// ContainerSecurityContext for the backup container.
	// +optional
	ContainerSecurityContext *corev1.SecurityContext `json:"containerSecurityContext,omitempty"`

	// PodLabels to add to the backup pod.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Resources for the backup container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// SuccessfulJobsHistoryLimit is how many completed Jobs to keep.
	// +kubebuilder:default:=3
	// +optional
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`

	// FailedJobsHistoryLimit is how many failed Jobs to keep.
	// +kubebuilder:default:=1
	// +optional
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`
}

// ScheduledBackupStatus is the observed state of a ScheduledBackup.
type ScheduledBackupStatus struct {
	// Conditions holds the latest observations of the backup's state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// EffectiveSchedule is the cron expression the shorthand resolved to.
	// +optional
	EffectiveSchedule string `json:"effectiveSchedule,omitempty"`

	// LastScheduleTime is when a backup was last started.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulTime is when a backup last succeeded.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// Active is how many backups are running.
	// +optional
	Active int32 `json:"active,omitempty"`

	// LastTriggerTime is the trigger-at annotation this operator has already acted on.
	// +optional
	LastTriggerTime *metav1.Time `json:"lastTriggerTime,omitempty"`

	// LastTriggerJob is the Job the last trigger created.
	// +optional
	LastTriggerJob string `json:"lastTriggerJob,omitempty"`

	// History is the most recent runs, newest first.
	// +listType=atomic
	// +optional
	History []BackupRun `json:"history,omitempty"`
}

// BackupRunResult is how a run ended.
// +kubebuilder:validation:Enum=Running;Succeeded;Failed
type BackupRunResult string

const (
	// BackupRunRunning means the run has not finished.
	BackupRunRunning BackupRunResult = "Running"
	// BackupRunSucceeded means the run completed.
	BackupRunSucceeded BackupRunResult = "Succeeded"
	// BackupRunFailed means the run failed.
	BackupRunFailed BackupRunResult = "Failed"
)

// BackupRunTrigger is what started a run.
// +kubebuilder:validation:Enum=Scheduled;Manual
type BackupRunTrigger string

const (
	// BackupTriggerScheduled means the CronJob started the run.
	BackupTriggerScheduled BackupRunTrigger = "Scheduled"
	// BackupTriggerManual means the trigger-at annotation started the run.
	BackupTriggerManual BackupRunTrigger = "Manual"
)

// BackupRun is one entry in the run history.
type BackupRun struct {
	// JobName is the Job that ran the backup.
	JobName string `json:"jobName"`

	// Trigger is what started the run.
	Trigger BackupRunTrigger `json:"trigger"`

	// Result is how the run ended.
	Result BackupRunResult `json:"result"`

	// StartTime is when the run started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run finished.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// ScheduledBackup is a recurring restic backup into a Repository.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Schedule",type="string",JSONPath=".status.effectiveSchedule"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Last Backup",type="date",JSONPath=".status.lastSuccessfulTime"
// +kubebuilder:printcolumn:name="Suspended",type="boolean",JSONPath=".spec.suspend"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="has(self.spec.sources) != has(self.spec.script)",message="exactly one of spec.sources or spec.script must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.sources) || !self.spec.sources.exists(s, s.type == 'files') || has(self.spec.volume)",message="a files source needs spec.volume; without it restic would back up the container's own filesystem"
type ScheduledBackup struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ScheduledBackupSpec `json:"spec"`

	// +optional
	Status ScheduledBackupStatus `json:"status,omitzero"`
}

// EffectiveResticHost returns the host recorded on this backup's snapshots.
func (s *ScheduledBackup) EffectiveResticHost() string {
	if s.Spec.ResticHost != "" {
		return s.Spec.ResticHost
	}
	return s.Namespace
}

// ScheduledBackupList is a list of ScheduledBackup.
// +kubebuilder:object:root=true
type ScheduledBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ScheduledBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ScheduledBackup{}, &ScheduledBackupList{})
		return nil
	})
}
