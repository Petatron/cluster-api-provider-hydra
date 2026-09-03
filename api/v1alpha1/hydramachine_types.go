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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// HydraImage identifies the base image a machine boots from.
//
// The image is named rather than described: Hydra resolves the name against
// whatever the backend already holds, and only falls back to url when it does
// not. Keeping resolution on the backend side is what stops libvirt storage-pool
// concepts leaking into an API that a Proxmox or bare-metal backend must also
// satisfy.
//
// +kubebuilder:validation:XValidation:rule="has(self.name) || has(self.url)",message="one of name or url must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.checksum) || has(self.url)",message="checksum is only meaningful alongside url"
type HydraImage struct {
	// name identifies a base image already known to the backend.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`

	// url is where the backend may fetch the image from when it does not already
	// hold one matching name.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url,omitempty"`

	// checksum verifies a fetched image, in the form "<algorithm>:<hex>".
	//
	// The digest length is pinned to the algorithm -- 64 hex characters for
	// sha256, 128 for sha512 -- so a truncated or malformed digest is rejected at
	// admission rather than discovered halfway through provisioning a machine.
	//
	// Only valid alongside url; a checksum with nothing to verify is rejected.
	// +optional
	// +kubebuilder:validation:Pattern=`^(sha256:[a-fA-F0-9]{64}|sha512:[a-fA-F0-9]{128})$`
	Checksum string `json:"checksum,omitempty"`
}

// HydraNetworkAttachment attaches a machine to one backend network.
type HydraNetworkAttachment struct {
	// name is the backend network or bridge to attach to, for example "br0".
	// Hydra does not interpret this value; it is passed to the backend, which
	// decides what it means.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// HydraMachineSpec defines the desired state of HydraMachine.
//
// Every field here is deliberately backend-neutral. A machine is described by
// what it must provide -- capacity, an image, network attachments -- and not by
// how any one backend provides it. Backend-specific configuration belongs to the
// backend, not to this API.
//
// Capacity, image and networks are immutable. Cluster API's model for changing a
// machine is to roll it out and replace it, not to mutate it in place, so
// enforcing that here turns a silently ignored edit into an immediate rejection.
//
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.providerID) || (has(self.providerID) && self.providerID == oldSelf.providerID)",message="providerID is immutable once set"
// +kubebuilder:validation:XValidation:rule="has(self.image) == has(oldSelf.image) && (!has(self.image) || self.image == oldSelf.image)",message="image is fixed when the machine is created; replace the machine instead"
// +kubebuilder:validation:XValidation:rule="has(self.networks) == has(oldSelf.networks) && (!has(self.networks) || self.networks == oldSelf.networks)",message="networks are fixed when the machine is created; replace the machine instead"
type HydraMachineSpec struct {
	// providerID is the unique identifier for this machine, in the form
	// hydra://<backend>/<id>. The controller sets it once the backing
	// infrastructure exists; the Kubernetes Node reports the same value, which is
	// how Cluster API pairs a Machine with its Node.
	//
	// This is written by the controller, not by users. It is immutable once set.
	//
	// NOTE: this field is part of the Cluster API contract.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ProviderID *string `json:"providerID,omitempty"`

	// vcpus is the number of virtual CPUs the machine is given.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="vcpus is immutable; replace the machine instead"
	VCPUs int32 `json:"vcpus"`

	// memory is the RAM the machine is given, as a Kubernetes quantity, for
	// example "8Gi". Must be greater than zero -- a zero or negative value would
	// otherwise reach libvirt as domain XML, and because this field is immutable
	// the resulting object could never be corrected, only deleted.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="memory is immutable; replace the machine instead"
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="memory must be greater than zero"
	Memory resource.Quantity `json:"memory"`

	// diskSize is the size of the machine's root disk, as a Kubernetes quantity,
	// for example "40Gi". Must be greater than zero, for the same reason as
	// memory.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="diskSize is immutable; replace the machine instead"
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="diskSize must be greater than zero"
	DiskSize resource.Quantity `json:"diskSize"`

	// image is the base image the machine boots from.
	//
	// Optional since the owning HydraCluster can supply a default. Omitting it
	// resolves in order: this field, then the cluster's baseImage, then the
	// manager's --libvirt-base-image flag. It is only an error when all three are
	// empty.
	//
	// Fixed when the machine is created, and that includes whether it is set at
	// all. Adding one later would be admitted while changing nothing: the VM was
	// already built from whatever this resolved to, and the reconciler only
	// re-reads a machine once its providerID exists -- so the object would claim
	// an image the running VM does not have. Cluster API replaces machines rather
	// than mutating them, which is the model this follows.
	//
	// Expressed on the spec rather than the field, because a field-level
	// transition rule cannot describe presence changing for an optional value.
	// +optional
	Image *HydraImage `json:"image,omitempty"`

	// networks are the backend networks this machine attaches to, in order. The
	// first attachment provides the machine's primary address.
	//
	// A list rather than a single value so that multi-homed machines do not
	// require an API break later.
	//
	// Optional for the same reason as image: the owning HydraCluster can supply
	// defaults. Unlike image there is no manager-flag fallback, and a machine
	// that resolves to no networks is refused -- it would boot, satisfy the
	// backend, and never reach an API server.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Networks []HydraNetworkAttachment `json:"networks,omitempty"`
}

// HydraMachineInitializationStatus provides observations of the HydraMachine
// initialization process.
//
// NOTE: fields in this struct are part of the Cluster API contract and are used
// to orchestrate initial Machine provisioning.
//
// +kubebuilder:validation:MinProperties=1
type HydraMachineInitializationStatus struct {
	// provisioned is true when the infrastructure backing this machine is fully
	// provisioned and the machine is ready to be bootstrapped.
	//
	// Cluster API reads this to populate the Machine's
	// status.initialization.infrastructureProvisioned, which is what actually
	// drives provisioning orchestration. Without it, Cluster API cannot observe
	// that this provider finished its work.
	//
	// NOTE: this field is part of the Cluster API contract.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// HydraMachineStatus defines the observed state of HydraMachine.
type HydraMachineStatus struct {
	// initialization reports whether the machine's infrastructure is provisioned.
	//
	// NOTE: this field is part of the Cluster API contract.
	// +optional
	Initialization HydraMachineInitializationStatus `json:"initialization,omitempty,omitzero"`

	// addresses are the addresses assigned to the machine. Cluster API surfaces
	// these on the Machine once initialization completes. They are not used to
	// drive any behaviour -- they exist so an operator debugging a stuck machine
	// can see where it actually landed.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// failureDomain is the failure domain the machine was actually placed in.
	//
	// Placement is requested at Machine level, not here; this field reports where
	// the backend put it, which is not necessarily the same thing.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	FailureDomain string `json:"failureDomain,omitempty"`

	// conditions represent the current state of the HydraMachine resource.
	//
	// A condition of type "Ready" is mirrored into the Machine's
	// InfrastructureReady condition, and is expected to describe the machine
	// across its whole lifecycle -- provisioning, steady state, and deletion --
	// not just whether provisioning finished once.
	//
	// Terminal failures are reported here too. The v1beta2 contract has no
	// special handling for them, so a well-documented condition type is the only
	// signal consumers get.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types owned by HydraMachine.
const (
	// MachineReadyCondition is mirrored into the Machine's InfrastructureReady
	// condition by Cluster API.
	MachineReadyCondition = "Ready"

	// MachinePausedCondition reports that reconciliation is suspended because the
	// object carries the Cluster API paused annotation.
	MachinePausedCondition = "Paused"

	// MachineProvisioningFailedCondition reports a failure the provider cannot
	// recover from on its own. The v1beta2 contract removed special treatment of
	// terminal failures, so this condition is the only way such a state reaches
	// an operator.
	MachineProvisioningFailedCondition = "ProvisioningFailed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:path=hydramachines,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned",description="Whether the backing infrastructure is fully provisioned"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Machine readiness across its lifecycle"
// +kubebuilder:printcolumn:name="ProviderID",type="string",JSONPath=".spec.providerID",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

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
