package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeletionPolicy controls what happens to the B2 data when the Repository is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain leaves the data, the application key and the credentials Secret in place.
	DeletionPolicyRetain DeletionPolicy = "Retain"

	// DeletionPolicyDelete removes the application key and every file version under the
	// repository's path, and in Bucket mode the bucket itself.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// StorageMode is how a repository is laid out in B2.
// +kubebuilder:validation:Enum=Bucket;Prefix
type StorageMode string

const (
	// StorageModeBucket gives the repository a bucket of its own, created if it does not exist.
	StorageModeBucket StorageMode = "Bucket"

	// StorageModePrefix puts the repository under its own path in an existing, shared bucket.
	StorageModePrefix StorageMode = "Prefix"
)

const (
	// RepositoryConditionReady reports whether the repository is usable.
	RepositoryConditionReady = "Ready"

	// RepositoryConditionInitialized reports whether restic init has run.
	RepositoryConditionInitialized = "Initialized"

	// RepositoryConditionLifecycleRule reports whether old file versions are being cleaned up.
	RepositoryConditionLifecycleRule = "LifecycleRule"
)

// DefaultDaysFromHidingToDeleting is the window the operator gives a path that no
// bucket rule covers.
const DefaultDaysFromHidingToDeleting int32 = 30

const (
	// CredentialsKeyID is the Secret key holding a B2 application key ID.
	CredentialsKeyID = "B2_APPLICATION_KEY_ID"

	// CredentialsKey is the Secret key holding a B2 application key.
	CredentialsKey = "B2_APPLICATION_KEY"
)

// LifecycleSpec is the bucket lifecycle rule the operator keeps for the repository.
type LifecycleSpec struct {
	// Manage lets the operator add a lifecycle rule for the repository's path,
	// so that files restic deletes, which B2 only hides, are removed for good.
	// Rules for other prefixes are never changed. Set false to leave the
	// bucket's rules alone. Defaults to true.
	// +optional
	Manage *bool `json:"manage,omitempty"`

	// DaysFromHidingToDeleting is how long a hidden file version is kept before
	// B2 deletes it.
	//
	// Unset, the operator adds no rule when a rule on the repository's path or
	// any parent of it already sets daysFromHidingToDeleting, whatever its
	// value; the LifecycleRule condition then reports CoveredByExistingRule.
	// Only a path no rule covers gets a rule, of 30 days.
	//
	// Set, the operator keeps a rule for exactly the path with this value even
	// under a parent rule. B2 applies the smallest value of every rule that
	// matches a file, so a value shorter than a parent rule's shortens the
	// window for this path and a longer one has no effect.
	// +kubebuilder:validation:Minimum=1
	// +optional
	DaysFromHidingToDeleting *int32 `json:"daysFromHidingToDeleting,omitempty"`
}

// RepositorySpec defines the desired state of a Repository.
type RepositorySpec struct {
	// Interval is how often the repository is reconciled against the B2 API.
	// +kubebuilder:default:="1h"
	// +kubebuilder:validation:XValidation:rule="self == '0' || self.matches('^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h))+$')",message="must be a Go duration such as 30m, 1h or 1h30m"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Suspend stops reconciliation without deleting the resource.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// DeletionPolicy decides whether the B2 data outlives this resource.
	// +kubebuilder:default:=Retain
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// Mode is how the repository is laid out. Bucket gives it a private bucket of
	// its own, created when missing and adopted when this account already has it.
	// Prefix puts it under its own path in an existing bucket that other
	// repositories share; the operator never creates or deletes that bucket.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="mode cannot be changed"
	// +required
	Mode StorageMode `json:"mode"`

	// Bucket is the B2 bucket name. Bucket names are global across all of B2.
	// +kubebuilder:validation:MinLength=6
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('b2-')",message="bucket names starting with b2- are reserved by Backblaze"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bucket cannot be changed"
	// +required
	Bucket string `json:"bucket"`

	// Path is where the repository lives inside the bucket, with no leading or
	// trailing slash. Defaults to the bucket root in Bucket mode and to
	// <namespace>/<name> in Prefix mode.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`
	// +kubebuilder:validation:XValidation:rule="!self.matches('(^|/)[.][.]?(/|$)')",message="path must not contain . or .. segments"
	// +optional
	Path string `json:"path,omitempty"`

	// Lifecycle is the bucket lifecycle rule kept for the repository's path.
	// +optional
	Lifecycle *LifecycleSpec `json:"lifecycle,omitempty"`

	// CredentialsSecretRef names a Secret in this namespace holding the B2
	// application key the operator manages this repository with, under
	// B2_APPLICATION_KEY_ID and B2_APPLICATION_KEY. Defaults to the operator's own.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`

	// PasswordSecretRef seeds the restic password. Required when the path already holds a repository.
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// SecretName overrides the name of the Secret the operator writes credentials to.
	// Defaults to <name>-credentials. It cannot be changed once the Repository
	// exists: backups, restores and key rotation all read the Secret by name.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// RepositoryStatus is the observed state of a Repository.
type RepositoryStatus struct {
	// Conditions holds the latest observations of the repository's state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// AccountID is the B2 account holding the bucket. Deletion treats a bucket
	// it cannot find as gone only when the credentials belong to this account.
	// +optional
	AccountID string `json:"accountID,omitempty"`

	// BucketName is the bucket holding the repository.
	// +optional
	BucketName string `json:"bucketName,omitempty"`

	// BucketID is the B2 ID of that bucket.
	// +optional
	BucketID string `json:"bucketID,omitempty"`

	// Path is the repository's path inside the bucket. Empty means the bucket root.
	// +optional
	Path string `json:"path,omitempty"`

	// Endpoint is the S3-compatible endpoint restic talks to.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ApplicationKeyID is the B2 application key written to the credentials Secret.
	// +optional
	ApplicationKeyID string `json:"applicationKeyID,omitempty"`

	// Adopted reports whether a restic repository already existed at the path.
	// +optional
	Adopted bool `json:"adopted,omitempty"`

	// Initialized reports whether restic init has completed.
	// +optional
	Initialized bool `json:"initialized,omitempty"`

	// SecretName is the Secret holding the repository credentials.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// CurrentUsage is the total size of the files restic can see under the path.
	// It excludes hidden versions still awaiting deletion. Empty when unknown.
	// +optional
	CurrentUsage string `json:"currentUsage,omitempty"`

	// UsageTime is when CurrentUsage was measured.
	// +optional
	UsageTime *metav1.Time `json:"usageTime,omitempty"`

	// KeyRotationTime is when the application key in the credentials Secret was
	// last replaced. Superseded keys are deleted no sooner than 10 minutes after
	// it, once no Job that started before it is still running, or 24 hours
	// after it at the latest.
	// +optional
	KeyRotationTime *metav1.Time `json:"keyRotationTime,omitempty"`

	// LastReinitTime is the reinit-at annotation the operator has already acted on.
	// +optional
	LastReinitTime *metav1.Time `json:"lastReinitTime,omitempty"`
}

// Repository is a restic repository stored in Backblaze B2.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Bucket",type="string",JSONPath=".status.bucketName"
// +kubebuilder:printcolumn:name="Path",type="string",JSONPath=".status.path"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Initialized",type="boolean",JSONPath=".status.initialized"
// +kubebuilder:printcolumn:name="Usage",type="string",JSONPath=".status.currentUsage"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="has(self.spec.path) == has(oldSelf.spec.path) && (!has(self.spec.path) || self.spec.path == oldSelf.spec.path)",message="path cannot be added, changed or removed once the Repository exists"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.secretName) && size(self.spec.secretName) > 0 ? self.spec.secretName : self.metadata.name + '-credentials') == (has(oldSelf.spec.secretName) && size(oldSelf.spec.secretName) > 0 ? oldSelf.spec.secretName : oldSelf.metadata.name + '-credentials')",message="secretName cannot be changed once the Repository exists; it may only be set to or cleared from the default <name>-credentials"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.passwordSecretRef) || self.spec.passwordSecretRef.name != (has(self.spec.secretName) && size(self.spec.secretName) > 0 ? self.spec.secretName : self.metadata.name + '-credentials')",message="secretName must not be the passwordSecretRef Secret; the operator never writes to the seed"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.credentialsSecretRef) || self.spec.credentialsSecretRef.name != (has(self.spec.secretName) && size(self.spec.secretName) > 0 ? self.spec.secretName : self.metadata.name + '-credentials')",message="secretName must not be the credentialsSecretRef Secret; the operator never writes to it"
type Repository struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec RepositorySpec `json:"spec"`

	// +optional
	Status RepositoryStatus `json:"status,omitzero"`
}

// SecretName returns the Secret the operator writes credentials to.
func (r *Repository) SecretName() string {
	if r.Spec.SecretName != "" {
		return r.Spec.SecretName
	}
	return r.Name + "-credentials"
}

// EffectivePath returns the path inside the bucket. Empty means the bucket root.
func (r *Repository) EffectivePath() string {
	if r.Spec.Path != "" {
		return r.Spec.Path
	}
	if r.Spec.Mode == StorageModePrefix {
		return r.Namespace + "/" + r.Name
	}
	return ""
}

// ManagesLifecycle reports whether the operator keeps a lifecycle rule for the repository.
func (r *Repository) ManagesLifecycle() bool {
	return r.Spec.Lifecycle == nil || r.Spec.Lifecycle.Manage == nil || *r.Spec.Lifecycle.Manage
}

// DaysFromHidingToDeleting returns how long hidden versions are kept by a rule the
// operator adds.
func (r *Repository) DaysFromHidingToDeleting() int32 {
	if r.ExplicitDaysFromHidingToDeleting() {
		return *r.Spec.Lifecycle.DaysFromHidingToDeleting
	}
	return DefaultDaysFromHidingToDeleting
}

// ExplicitDaysFromHidingToDeleting reports whether spec.lifecycle.daysFromHidingToDeleting is set.
func (r *Repository) ExplicitDaysFromHidingToDeleting() bool {
	return r.Spec.Lifecycle != nil && r.Spec.Lifecycle.DaysFromHidingToDeleting != nil
}

// RepositoryList is a list of Repository.
// +kubebuilder:object:root=true
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Repository `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Repository{}, &RepositoryList{})
		return nil
	})
}
