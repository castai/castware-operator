package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CastwareSpec defines the desired state of Castware.
type CastwareSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// CastwareStatus defines the observed state of Castware.
type CastwareStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Castware is the Schema for the castwares API.
type Castware struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CastwareSpec   `json:"spec,omitempty"`
	Status CastwareStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CastwareList contains a list of Castware.
type CastwareList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Castware `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Castware{}, &CastwareList{})
}
