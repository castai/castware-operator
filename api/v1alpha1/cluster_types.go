// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ClusterSpec defines the desired state of Cluster
type ClusterSpec struct {
	// Provider is the cloud provider where Castware is installed.
	//+optional
	Provider string `json:"provider"`

	// APIKeySecret is the name of the Kubernetes Secret containing the Mothership API key.
	// The operator does not work without it.
	//+kubebuilder:validation:Required
	//+kubebuilder:default:="castware-api-key"
	APIKeySecret string `json:"apiKeySecret"`

	// API holds all the Mothership API endpoints.
	// Only apiUrl is required; the others are derived if omitted.
	// +kubebuilder:validation:Required
	API APISpec `json:"api"`

	// Cluster contains metadata that is populated after the agent registers.
	// All fields here are optional; if missing, the operator will fill them in at runtime.
	// +optional
	Cluster *ClusterMetadataSpec `json:"cluster,omitempty"`
}

// APISpec groups HTTP/gRPC endpoints for Mothership and related services.
// - apiUrl is required (Format=uri).
// - All others are optional and may be derived by your controller if omitted.
type APISpec struct {
	// apiUrl is the “base” HTTP URL of the Mothership API.
	// +kubebuilder:validation:Format=uri
	// +kubebuilder:validation:Required
	APIURL string `json:"apiUrl,omitempty"`

	// apiGrpcUrl is the gRPC endpoint for the Mothership API (optional).
	// +optional
	APIGrpcURL string `json:"apiGrpcUrl,omitempty"`

	// grpcUrl is a generic gRPC endpoint for any Mothership service (optional).
	// +optional
	GrpcURL string `json:"grpcUrl,omitempty"`

	// kvisorGrpcUrl is the gRPC endpoint for the Kvisor service (optional).
	// +optional
	KvisorGrpcURL string `json:"kvisorGrpcUrl,omitempty"`
}

// ClusterMetadataSpec holds basic cluster metadata. All fields are optional because they
// will be filled in by the operator only after the agent “checks in.”
type ClusterMetadataSpec struct {
	// ClusterID is the unique UUID of this cluster in the Cast AI database.
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89ABab][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// ClusterName is a human-readable name for this cluster (optional).
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// Location is the region or zone of the cluster (optional).
	// +optional
	Location string `json:"location,omitempty"`

	// ProjectID is the cloud-provider project/subscription/account ID (optional).
	// +optional
	ProjectID string `json:"projectID,omitempty"`
}

// ClusterStatus defines the observed state of Cluster
type ClusterStatus struct {
	// Conditions store the status conditions of the Cluster instances
	// +operator-sdk:csv:customresourcedefinitions:type=status
	Conditions        []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	LastSecretVersion string             `json:"lastSecretVersion,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Cluster is the Schema for the clusters API
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Cluster{}, &ClusterList{})
}
