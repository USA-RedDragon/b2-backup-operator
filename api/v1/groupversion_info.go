// Package v1 contains the b2backup.mcswain.dev/v1 API types.
// +kubebuilder:object:generate=true
// +groupName=b2backup.mcswain.dev
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion is the group version used to register these objects.
	SchemeGroupVersion = schema.GroupVersion{Group: "b2backup.mcswain.dev", Version: "v1"}

	// GroupVersion is an alias for SchemeGroupVersion.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder collects the functions that add these types to a Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToScheme adds these types to a Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
