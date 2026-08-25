/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// HydraMachineTemplateResource is the machine payload a MachineDeployment clones
// when it stamps out a new HydraMachine.
type HydraMachineTemplateResource struct {
	// metadata is the object metadata applied to machines cloned from this template.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the HydraMachine spec applied to machines cloned from this template.
	Spec HydraMachineSpec `json:"spec"`
}

// HydraMachineTemplateSpec defines the desired state of HydraMachineTemplate.
//
// The nesting here is not stylistic: the Cluster API v1beta2 InfraMachineTemplate
// contract requires the machine payload to live at spec.template.spec. Cluster API
// clones that subtree verbatim into the InfraMachine it creates. Flattening it would
// make this template uncloneable, and MachineDeployments referencing it would fail.
type HydraMachineTemplateSpec struct {
	// template is the machine payload cloned into each HydraMachine.
	Template HydraMachineTemplateResource `json:"template"`
}

// HydraMachineTemplateStatus defines the observed state of HydraMachineTemplate.
type HydraMachineTemplateStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the HydraMachineTemplate resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// HydraMachineTemplate is the Schema for the hydramachinetemplates API
type HydraMachineTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of HydraMachineTemplate
	// +required
	Spec HydraMachineTemplateSpec `json:"spec"`

	// status defines the observed state of HydraMachineTemplate
	// +optional
	Status HydraMachineTemplateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HydraMachineTemplateList contains a list of HydraMachineTemplate
type HydraMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HydraMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &HydraMachineTemplate{}, &HydraMachineTemplateList{})
		return nil
	})
}
