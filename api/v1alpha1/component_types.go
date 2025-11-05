package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

const (
	ComponentMigrationHelm      = "helm"
	ComponentMigrationYaml      = "yaml"
	ComponentMigrationTerraform = "terraform"

	LabelHelmChart = "castware.cast.ai/helm-chart"
)

// ComponentSpec defines the desired state of Component
type ComponentSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Component is the name of the component represented in this CRD.
	//+kubebuilder:validation:Required
	//+kubebuilder:default:="castai-agent"
	Component string `json:"component"`

	// Cluster is the name of the cluster CRD containing global parameters for this component.
	//+kubebuilder:validation:Required
	//+kubebuilder:default:="castai"
	Cluster string `json:"cluster"`

	// Enabled is set to false if this component should be scaled to zero replicas.
	//+kubebuilder:validation:Required
	Enabled bool `json:"enabled"`

	// Version is the the version of the helm chart that should be installed, if not specified
	// the latest version will be installed and the value will be filled by the operator.
	//+optional
	Version string `json:"version"`

	// Values is a free-form map of Helm values (exactly like a values.yaml block).
	// The operator will pass these to `helm install/upgrade --values`.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// Migration tells the operator if there is an existing component to migrate
	// or just a new component to install.
	//+kubebuilder:validation:Enum=yaml;helm;""
	//+kubebuilder:default:=""
	Migration string `json:"migration,omitempty"`

	// Readonly tells the operator if the component can be modified.
	//+kubebuilder:default:=false
	Readonly bool `json:"readonly,omitempty"`
}

// ComponentStatus defines the observed state of Component
type ComponentStatus struct {
	// Conditions store the status conditions of the Component instances
	// +operator-sdk:csv:customresourcedefinitions:type=status
	Conditions     []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	CurrentVersion string             `json:"currentVersion,omitempty" protobuf:"string,2,rep,name=currentVersion"`
	// Set it to true if the component should be rolled back to the previous version,
	// the reconcile loop will set it to false when the rollback starts.
	Rollback bool `json:"rollback,omitempty" protobuf:"bool,3,rep,name=rollback"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Component is the Schema for the components API
type Component struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ComponentSpec   `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

func (c Component) HelmChartName() string {
	if c.Labels != nil && c.Labels[LabelHelmChart] != "" {
		return c.Labels[LabelHelmChart]
	}
	// Fallback to the component name if there is no helm chart label.
	return c.Spec.Component
}

//+kubebuilder:object:root=true

// ComponentList contains a list of Component
type ComponentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Component `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Component{}, &ComponentList{})
}
