// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourcePhase represents the lifecycle state of a Resource.
// +kubebuilder:validation:Enum=Provisioning;Ready;Failed
type ResourcePhase string

const (
	// ResourcePhaseProvisioning indicates the resource is being set up.
	ResourcePhaseProvisioning ResourcePhase = "Provisioning"

	// ResourcePhaseReady indicates the resource is active and healthy.
	ResourcePhaseReady ResourcePhase = "Ready"

	// ResourcePhaseFailed indicates the resource has encountered an unrecoverable error.
	ResourcePhaseFailed ResourcePhase = "Failed"
)

// ResourceSpec defines the desired state of Resource.
type ResourceSpec struct {
	// Description is a human-readable description of this resource.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=256
	Description string `json:"description,omitempty"`
}

// ResourceStatus defines the observed state of Resource.
type ResourceStatus struct {
	// Phase represents the current lifecycle phase of the resource.
	//
	// +kubebuilder:validation:Optional
	Phase ResourcePhase `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the resource's state.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Resource is the Schema for the resources API.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// Milo-specific: update or remove this annotation for your resource type
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Organization"
type Resource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceSpec   `json:"spec,omitempty"`
	Status ResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceList contains a list of Resource.
type ResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Resource `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Resource{}, &ResourceList{})
}
