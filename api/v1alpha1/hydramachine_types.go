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
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// HydraMachineSpec defines the desired state of HydraMachine.
//
// Only the fields mandated by the Cluster API v1beta2 InfraMachine contract are
// present. Hydra's own fields (machine class, image, network, GPU) are PET-7.
type HydraMachineSpec struct {
	// providerID is the unique identifier for this machine, in the form
	// hydra://<backend>/<id>. The controller sets it once the backing
	// infrastructure exists; the Kubernetes Node reports the same value, which
	// is how Cluster API pairs a Machine with its Node.
	//
	// NOTE: this field is part of the Cluster API contract.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`
}

// HydraMachineInitializationStatus reports provisioning progress to Cluster API.
type HydraMachineInitializationStatus struct {
	// provisioned is true when the infrastructure backing this machine is fully
	// provisioned and the machine is ready to be bootstrapped.
	//
	// NOTE: this field is part of the Cluster API contract. Cluster API reads it
	// to populate Machine.status.initialization.infrastructureProvisioned, which
	// is what actually drives provisioning orchestration. Without it, Cluster API
	// cannot observe that this provider finished its work.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// HydraMachineStatus defines the observed state of HydraMachine.
type HydraMachineStatus struct {
	// initialization reports whether the machine's infrastructure is provisioned.
	//
	// NOTE: this field is part of the Cluster API contract.
	// +optional
	Initialization HydraMachineInitializationStatus `json:"initialization,omitempty"`

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the HydraMachine resource.
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

// HydraMachine is the Schema for the hydramachines API
type HydraMachine struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of HydraMachine
	// +required
	Spec HydraMachineSpec `json:"spec"`

	// status defines the observed state of HydraMachine
	// +optional
	Status HydraMachineStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HydraMachineList contains a list of HydraMachine
type HydraMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HydraMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &HydraMachine{}, &HydraMachineList{})
		return nil
	})
}
