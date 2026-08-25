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

// HydraMachineTemplateResource is the machine payload a MachineDeployment clones
// when it stamps out a new HydraMachine.
type HydraMachineTemplateResource struct {
	// metadata is the object metadata applied to machines cloned from this
	// template.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the HydraMachine spec applied to machines cloned from this
	// template.
	//
	// providerID must not be set here. It identifies one specific machine, and a
	// template describes many; a value here would be copied into every clone and
	// collide immediately.
	// +required
	// +kubebuilder:validation:XValidation:rule="!has(self.providerID)",message="providerID must not be set on a template; it identifies a single machine"
	Spec HydraMachineSpec `json:"spec"`
}

// HydraMachineTemplateSpec defines the desired state of HydraMachineTemplate.
//
// The nesting is not stylistic: the Cluster API v1beta2 InfraMachineTemplate
// contract requires the machine payload at spec.template.spec, and Cluster API
// clones that subtree verbatim into the HydraMachine it creates. Flattening it
// would make the template uncloneable, and a MachineDeployment referencing it
// would fail at reconcile time rather than at apply time.
//
// The whole spec is immutable. Cluster API's model is that changing a machine
// shape means creating a new template and rolling the MachineDeployment onto it,
// which keeps a template an accurate record of what its existing machines were
// built from.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="HydraMachineTemplate spec is immutable; create a new template and roll the MachineDeployment onto it"
type HydraMachineTemplateSpec struct {
	// template is the machine payload cloned into each HydraMachine.
	// +required
	Template HydraMachineTemplateResource `json:"template"`
}

// HydraMachineTemplateStatus defines the observed state of HydraMachineTemplate.
//
// Intentionally minimal. The Cluster API contract also allows a template to
// report status.capacity and status.nodeInfo, which is how Cluster Autoscaler
// sizes a node pool that currently has zero replicas -- there is no Node to
// inspect, so the capacity has to come from the template. That is PET-27, and
// adding those fields later is an additive, non-breaking change.
type HydraMachineTemplateStatus struct {
	// conditions represent the current state of the HydraMachineTemplate
	// resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:path=hydramachinetemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="vCPUs",type="integer",JSONPath=".spec.template.spec.vcpus"
// +kubebuilder:printcolumn:name="Memory",type="string",JSONPath=".spec.template.spec.memory"
// +kubebuilder:printcolumn:name="Disk",type="string",JSONPath=".spec.template.spec.diskSize"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.template.spec.image.name",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

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
