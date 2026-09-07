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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

// HydraNodeCapacity is the resource capacity of a node, keyed by resource name.
//
// Structurally a corev1.ResourceList, and identical on the wire, but declared
// here so the bound and the validation rule below can be attached to it. Neither
// marker can be applied to an imported named map type, and without a bound the
// API server rejects the whole CRD: a rule that walks an unbounded map blows the
// CEL cost budget by more than 100x.
//
// +kubebuilder:validation:MaxProperties=32
// +kubebuilder:validation:XValidation:rule="self.all(k, type(self[k]) == string)",message="capacity values must be strings; Cluster Autoscaler reads this map as strings and silently discards all of it otherwise"
type HydraNodeCapacity map[corev1.ResourceName]resource.Quantity

// HydraNodeArchitecture is the CPU architecture of the node a machine becomes.
//
// The value set is fixed by the Cluster API contract, not by us: Cluster
// Autoscaler copies it into the simulated Node's kubernetes.io/arch label, and
// a value outside this set would produce a label no pod nodeSelector matches.
//
// +kubebuilder:validation:Enum=amd64;arm64;s390x;ppc64le
type HydraNodeArchitecture string

const (
	HydraNodeArchitectureAMD64 HydraNodeArchitecture = "amd64"
	HydraNodeArchitectureARM64 HydraNodeArchitecture = "arm64"
)

// HydraNodeInfo describes the platform of the node a machine cloned from this
// template becomes.
//
// It exists for scale-from-zero: with no Node to inspect, Cluster Autoscaler
// derives the simulated node's kubernetes.io/arch and kubernetes.io/os labels
// from here. Omitting it is not neutral -- the autoscaler falls back to
// CAPI_SCALE_ZERO_DEFAULT_ARCH or amd64/linux, so an arm64 pool with no
// nodeInfo simulates as amd64 and schedules pods that cannot run.
//
// NOTE: this struct is part of the Cluster API InfraMachineTemplate contract.
//
// +kubebuilder:validation:MinProperties=1
type HydraNodeInfo struct {
	// architecture is the CPU architecture of the node.
	// +optional
	Architecture HydraNodeArchitecture `json:"architecture,omitempty"`

	// operatingSystem is the operating system the node runs, for example "linux".
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	OperatingSystem string `json:"operatingSystem,omitempty"`
}

// HydraMachineTemplateStatus defines the observed state of HydraMachineTemplate.
type HydraMachineTemplateStatus struct {
	// capacity is the resource capacity of a node created from this template.
	//
	// This is how Cluster Autoscaler sizes a node pool that has zero replicas:
	// with no Node to inspect, the capacity has to come from the template. cpu
	// and memory are both required for scale-from-zero to work at all -- the
	// autoscaler skips a node group that cannot report both.
	//
	// The values are raw machine sizing, and are an UPPER BOUND on what the node
	// offers rather than a prediction of it. The autoscaler simulates a node whose
	// allocatable equals capacity; a real node is lower both because the guest
	// sees less RAM and disk than the machine was given and because the kubelet
	// holds back its eviction thresholds. Measured against a live 2 vCPU / 4Gi /
	// 40Gi Ubuntu 24.04 node at 6.8% over allocatable memory and 15.2% over
	// allocatable ephemeral storage -- docs/scale-from-zero.md carries the full
	// figures and the annotations that override this per pool.
	//
	// Every value must be a JSON string. The autoscaler reads this map with
	// unstructured.NestedStringMap, which fails whole rather than per-key: a
	// single integer value -- which the quantity schema would otherwise admit --
	// drops the entire capacity map and disables scale-from-zero with no error
	// reported anywhere. The validation rule below refuses that write instead.
	//
	// NOTE: this field is part of the Cluster API InfraMachineTemplate contract.
	// +optional
	Capacity HydraNodeCapacity `json:"capacity,omitempty"`

	// nodeInfo describes the platform of a node created from this template.
	//
	// NOTE: this field is part of the Cluster API InfraMachineTemplate contract.
	// +optional
	NodeInfo HydraNodeInfo `json:"nodeInfo,omitempty,omitzero"`

	// conditions represent the current state of the HydraMachineTemplate
	// resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types owned by HydraMachineTemplate.
const (
	// MachineTemplatePausedCondition reports that reconciliation is suspended
	// because the object, or the Cluster that owns it, carries the Cluster API
	// paused annotation.
	MachineTemplatePausedCondition = "Paused"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:path=hydramachinetemplates,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="vCPUs",type="integer",JSONPath=".spec.template.spec.vcpus"
// +kubebuilder:printcolumn:name="Memory",type="string",JSONPath=".spec.template.spec.memory"
// +kubebuilder:printcolumn:name="Disk",type="string",JSONPath=".spec.template.spec.diskSize"
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.template.spec.image.name",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Reported CPU",type="string",JSONPath=".status.capacity.cpu",priority=1,description="Capacity published for scale-from-zero; empty means Cluster Autoscaler cannot size this pool at zero replicas"
// +kubebuilder:printcolumn:name="Reported Memory",type="string",JSONPath=".status.capacity.memory",priority=1
// +kubebuilder:printcolumn:name="Arch",type="string",JSONPath=".status.nodeInfo.architecture",priority=1

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
