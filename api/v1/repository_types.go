package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeletionPolicy controls what happens to the BorgBase repository when the Repository is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain leaves the BorgBase repository in place.
	DeletionPolicyRetain DeletionPolicy = "Retain"

	// DeletionPolicyDelete removes the BorgBase repository and its snapshots.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

const (
	// RepositoryConditionReady reports whether the repository is usable.
	RepositoryConditionReady = "Ready"

	// RepositoryConditionInitialized reports whether restic init has run.
	RepositoryConditionInitialized = "Initialized"
)

// RepositorySpec defines the desired state of a Repository.
type RepositorySpec struct {
	// Interval is how often the repository is reconciled against the BorgBase API.
	// +kubebuilder:default:="1h"
	// +kubebuilder:validation:XValidation:rule="self == '0' || self.matches('^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h))+$')",message="must be a Go duration such as 30m, 1h or 1h30m"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// Suspend stops reconciliation without deleting the resource.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// DeletionPolicy decides whether the BorgBase repository outlives this resource.
	// +kubebuilder:default:=Retain
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// APITokenSecretRef points at the BorgBase API token. Defaults to the operator's own token.
	// +optional
	APITokenSecretRef *corev1.SecretKeySelector `json:"apiTokenSecretRef,omitempty"`

	// ExistingRepositoryID adopts an existing BorgBase repository instead of creating one.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]{4,32}$`
	// +optional
	ExistingRepositoryID string `json:"existingRepositoryID,omitempty"`

	// PasswordSecretRef seeds the restic password. Required when adopting.
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// RepositoryName is the name given to the repository in BorgBase. Defaults to the namespace.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	RepositoryName string `json:"repositoryName,omitempty"`

	// Region is the BorgBase region to create the repository in.
	// +kubebuilder:default:="us"
	// +optional
	Region string `json:"region,omitempty"`

	// QuotaGiB caps repository size. Unset means no quota.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000000
	// +optional
	QuotaGiB *int32 `json:"quotaGiB,omitempty"`

	// AlertDays is how many quiet days pass before BorgBase alerts. Unset leaves the
	// repository's current setting.
	// +kubebuilder:validation:Minimum=0
	// +optional
	AlertDays *int32 `json:"alertDays,omitempty"`

	// AppendOnly makes the repository reject deletes and overwrites.
	// +optional
	AppendOnly bool `json:"appendOnly,omitempty"`

	// SecretName overrides the name of the Secret the operator writes credentials to.
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

	// RepositoryID is the BorgBase repository ID.
	// +optional
	RepositoryID string `json:"repositoryID,omitempty"`

	// Adopted reports whether the repository pre-existed this resource.
	// +optional
	Adopted bool `json:"adopted,omitempty"`

	// Initialized reports whether restic init has completed.
	// +optional
	Initialized bool `json:"initialized,omitempty"`

	// Server is the host serving the repository over REST.
	// +optional
	Server string `json:"server,omitempty"`

	// SecretName is the Secret holding the repository credentials.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// CurrentUsage is the human-readable size of the repository.
	// +optional
	CurrentUsage string `json:"currentUsage,omitempty"`

	// Quota is the human-readable quota, if one is set.
	// +optional
	Quota string `json:"quota,omitempty"`
}

// Repository is a restic repository hosted on BorgBase.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo ID",type="string",JSONPath=".status.repositoryID"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Initialized",type="boolean",JSONPath=".status.initialized"
// +kubebuilder:printcolumn:name="Usage",type="string",JSONPath=".status.currentUsage"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.existingRepositoryID) || has(self.spec.passwordSecretRef)",message="passwordSecretRef is required when adopting an existing repository, otherwise its snapshots would be unreadable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.existingRepositoryID) || (has(self.spec.existingRepositoryID) && self.spec.existingRepositoryID == oldSelf.spec.existingRepositoryID)",message="existingRepositoryID cannot be changed or removed once set"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.spec.existingRepositoryID) || !has(self.spec.existingRepositoryID) || !has(oldSelf.status) || !has(oldSelf.status.repositoryID)",message="cannot adopt a different repository after one has been created for this resource"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.passwordSecretRef) || !has(self.spec.secretName) || self.spec.secretName != self.spec.passwordSecretRef.name",message="secretName must not be the passwordSecretRef Secret; the operator never writes to the seed"
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
	return r.Name + "-b2"
}

// RepositoryName returns the name to give the repository in BorgBase.
func (r *Repository) RepositoryName() string {
	if r.Spec.RepositoryName != "" {
		return r.Spec.RepositoryName
	}
	return r.Namespace
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
