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

// HydraClusterSpec defines the desired state of HydraCluster.
//
// This is the cluster-scoped half of the provider's configuration. Everything
// here was previously a process-wide flag on the manager, which meant one
// storage pool and one default image for every cluster a single controller
// managed. Moving it onto an object per cluster is the entire point of this
// type -- the Cluster API contract fields below are the smaller part of its job.
//
// Note what is deliberately absent: how to reach the hypervisor. That is still
// a manager flag, and moving it here is PET-38, bundled with the switch to TLS.
// It is left out rather than stubbed because a field the provider accepts and
// ignores is worse than one that does not exist -- see spec.image on
// HydraMachine before PET-8's review caught it.
// +kubebuilder:validation:XValidation:rule="has(self.controlPlaneEndpoint) && size(self.controlPlaneEndpoint.host) > 0 && self.controlPlaneEndpoint.port > 0",message="controlPlaneEndpoint requires both a host and a non-zero port"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.storagePool) || (has(self.storagePool) && self.storagePool == oldSelf.storagePool)",message="storagePool is immutable once set; existing machines were cloned from a volume inside it"
type HydraClusterSpec struct {
	// controlPlaneEndpoint is the address and port of this cluster's API server.
	//
	// Cluster API reads it to populate the Cluster's own endpoint. Providers that
	// create the endpoint themselves -- with a load balancer or a VIP -- are
	// expected to fill it in. Hydra creates neither, so this is supplied by
	// whoever writes the object, and the provider only reports it back.
	//
	// Immutable once set. Moving a live cluster's API endpoint is not something
	// a reconcile can carry out; it is a new cluster.
	//
	// Required in practice, and enforced by a rule on the spec rather than by
	// +required on this field. The contract types make host and port individually
	// optional and the struct itself omitempty, so +required would still admit
	// {host: "x"} with port 0 -- an endpoint that satisfies the schema and cannot
	// be dialled. Reporting infrastructure provisioned with no usable endpoint
	// leaves Cluster API with nothing to copy and kubeadm with nowhere to join.
	//
	// NOTE: this field is part of the Cluster API contract.
	// +optional
	// +kubebuilder:validation:XValidation:rule="!has(oldSelf.host) || (has(self.host) && self.host == oldSelf.host)",message="controlPlaneEndpoint.host is immutable once set"
	// +kubebuilder:validation:XValidation:rule="!has(oldSelf.port) || (has(self.port) && self.port == oldSelf.port)",message="controlPlaneEndpoint.port is immutable once set"
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`

	// storagePool is the backend storage pool this cluster's machine disks are
	// created in, and where its base image is expected to live.
	//
	// Cluster-scoped on purpose, with no per-machine override. A pool per machine
	// is the arrangement found on the reference hardware -- one pool each for
	// three hand-built VMs -- and it is precisely the shape this provider cannot
	// use, because it resolves the base image as a volume *inside* the configured
	// pool. One pool per cluster keeps a single place the image can be.
	//
	// Empty falls back to the manager's --libvirt-storage-pool flag. When both
	// are empty the failure is reported terminally against this object, which is
	// the one that can be fixed -- the backend deliberately no longer refuses to
	// start over an unset pool, because that made the flag mandatory process-wide
	// and so made this field pointless.
	//
	// Immutable once set, enforced on the spec. A reconcile cannot carry out the
	// change it implies: existing machines were cloned from a backing volume
	// inside the old pool and keep referring to it, while new machines would land
	// somewhere else -- and provisioned does not regress, so a pool swapped for a
	// missing one would not even be reported.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	StoragePool string `json:"storagePool,omitempty"`

	// baseImage is the image machines in this cluster boot from when they do not
	// name one of their own.
	//
	// Empty falls back to the manager's --libvirt-base-image flag.
	// +optional
	BaseImage *HydraImage `json:"baseImage,omitempty"`

	// networks are the network attachments machines in this cluster receive when
	// they do not name any of their own.
	//
	// There is no fallback for this one. A machine with no network attachment
	// boots and can never reach an API server, so provisioning is refused rather
	// than producing something that looks healthy to the backend and is useless.
	//
	// The map-list markers mirror HydraMachine's, so a duplicate attachment name
	// is rejected here too. Without them an inherited duplicate would produce two
	// NICs on one attachment, while the identical list written directly on a
	// machine would be refused -- the same list admitted or rejected depending
	// only on which object it was written on.
	//
	// MinItems is deliberately NOT mirrored. This list is a default, and empty is
	// a meaningful value: "machines in this cluster name their own attachments".
	// Requiring one would make that arrangement inexpressible.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=8
	Networks []HydraNetworkAttachment `json:"networks,omitempty"`
}

// HydraClusterInitializationStatus reports Cluster API initialization
// milestones for the cluster's infrastructure.
type HydraClusterInitializationStatus struct {
	// provisioned reports that the cluster-scoped infrastructure this provider
	// depends on has been verified.
	//
	// Cluster API reads this to populate the Cluster's
	// status.initialization.infrastructureProvisioned, which gates machine
	// creation. Until it is true, no HydraMachine in this cluster is created.
	//
	// NOTE: this field is part of the Cluster API contract.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// HydraClusterStatus defines the observed state of HydraCluster.
type HydraClusterStatus struct {
	// initialization reports whether the cluster's infrastructure is ready.
	//
	// NOTE: this field is part of the Cluster API contract.
	// +optional
	Initialization HydraClusterInitializationStatus `json:"initialization,omitempty,omitzero"`

	// conditions represent the current state of the HydraCluster resource.
	//
	// A condition of type "Ready" is mirrored into the Cluster's
	// InfrastructureReady condition.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types owned by HydraCluster.
const (
	// ClusterReadyCondition is mirrored into the Cluster's InfrastructureReady
	// condition by Cluster API.
	ClusterReadyCondition = "Ready"

	// ClusterPausedCondition reports that reconciliation is suspended, either by
	// the paused annotation on this object or by the owning Cluster.
	ClusterPausedCondition = "Paused"

	// ClusterInfrastructureFailedCondition reports that the cluster-scoped
	// prerequisites could not be satisfied and will not fix themselves -- a
	// storage pool that does not exist, or a base image missing from it.
	//
	// Separate from Ready because the two answer different questions: Ready is
	// "can machines be created right now", this is "is someone going to have to
	// intervene". Only the second should invite an operator's attention.
	ClusterInfrastructureFailedCondition = "InfrastructureFailed"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:path=hydraclusters,scope=Namespaced,categories=cluster-api
// +kubebuilder:printcolumn:name="Provisioned",type="boolean",JSONPath=".status.initialization.provisioned",description="Whether cluster infrastructure has been verified"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Pool",type="string",JSONPath=".spec.storagePool",priority=1
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.controlPlaneEndpoint.host",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// HydraCluster is the Schema for the hydraclusters API.
//
// It is referenced by a Cluster's spec.infrastructureRef, which is what makes
// the Cluster admissible at all: Cluster API rejects a Cluster carrying none of
// infrastructureRef, controlPlaneRef or topology.
type HydraCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of HydraCluster
	// +required
	Spec HydraClusterSpec `json:"spec"`

	// status defines the observed state of HydraCluster
	// +optional
	Status HydraClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HydraClusterList contains a list of HydraCluster
type HydraClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HydraCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &HydraCluster{}, &HydraClusterList{})
		return nil
	})
}
