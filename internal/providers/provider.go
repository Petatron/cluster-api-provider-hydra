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

// Package providers defines the boundary between Hydra's Cluster API-facing
// controllers and the infrastructure backends that actually create machines.
//
// The controller owns everything Cluster API cares about: contracts, conditions,
// status, finalizers, owner references, providerID publication. A backend owns
// exactly one thing -- turning a MachineSpec into a running machine, and back.
//
// Nothing in this package may reference a specific backend. If a type here needs
// to know it is talking to libvirt, the boundary is in the wrong place.
package providers

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is returned by Get and FindByName when the machine does not exist.
//
// Delete never returns it: deleting an absent machine is the desired end state
// during teardown, so it reports success. A provider that returned ErrNotFound
// from Delete would wedge finalizers forever.
var ErrNotFound = errors.New("machine not found")

// ErrInvalidID marks a machine ID the backend can never resolve, as opposed to
// one that merely does not exist right now.
//
// providerID is writable at creation, so a value can be syntactically valid to
// the controller and still be meaningless to the backend. Without a distinct
// signal, deletion would treat that as a real failure and wedge the finalizer
// permanently -- even though the backend name can still identify and clean up
// the machine's resources.
var ErrInvalidID = errors.New("machine id is not valid for this backend")

// ErrTerminal marks a failure the provider cannot recover from by retrying.
//
// Backends wrap it around configuration-shaped errors -- a missing image, a
// nonexistent storage pool -- as distinct from an unreachable hypervisor, which
// is expected to succeed on a later attempt.
//
// The distinction has teeth: the controller only raises its terminal
// ProvisioningFailed condition for these, and a MachineHealthCheck or an
// operator may act on that condition. Reporting a transient network blip as
// terminal would invite remediation of a machine that was about to be fine.
var ErrTerminal = errors.New("terminal provider failure")

// AddressType classifies a machine address, mirroring the Cluster API
// MachineAddress types the controller surfaces on status.
type AddressType string

const (
	AddressTypeHostname   AddressType = "Hostname"
	AddressTypeInternalIP AddressType = "InternalIP"
	AddressTypeExternalIP AddressType = "ExternalIP"
)

// Address is one address assigned to a machine.
type Address struct {
	Type    AddressType
	Address string
}

// Image identifies the base image a machine boots from. Resolution is the
// backend's problem: Name refers to something the backend already holds, and
// URL is a fallback source for backends that can fetch.
type Image struct {
	Name     string
	URL      string
	Checksum string
}

// Network is one network attachment. Name is backend-defined and passed through
// uninterpreted -- a libvirt bridge, a Proxmox vmbr, whatever the backend means.
type Network struct {
	Name string
}

// MachineSpec is the backend-neutral description of a machine to create.
//
// Sizes are in bytes rather than Kubernetes quantities: quantities belong to the
// API surface, and converting once at the boundary keeps unit handling out of
// every backend.
type MachineSpec struct {
	// Name is the machine's identity on the backend, and backends must treat it
	// as the idempotency key -- see MachineProvider.Create.
	//
	// It must be GLOBALLY UNIQUE on the backend, not merely unique within a
	// Kubernetes namespace. A libvirt domain name is flat: two HydraMachines
	// called "worker-1" in different namespaces would otherwise adopt, and then
	// delete, each other's VM. The controller therefore derives this from
	// namespace, name and object UID rather than from the object name alone.
	Name string

	VCPUs       int32
	MemoryBytes int64
	DiskBytes   int64

	Image    Image
	Networks []Network

	// Hostname is the name the machine should call itself, and through it the
	// name it registers in Kubernetes under.
	//
	// It is separate from Name because the two answer different questions. Name
	// must be globally unique on the backend and is derived from a hash, which
	// makes it unsuitable as a node name; Hostname is the human-facing identity
	// and comes from the Cluster API Machine.
	//
	// A backend that cannot set a hostname may ignore this, but should expect
	// every machine cloned from one base image to answer to the same name.
	Hostname string

	// BootstrapData is cloud-init user-data, attached to the machine so it can
	// configure itself on first boot.
	//
	// Empty is a supported state, not a missing value: a machine created with no
	// bootstrap data boots but never joins a cluster, which is how the
	// infrastructure half is exercised without Cluster API in the picture.
	//
	// Backends that cannot deliver it MUST fail rather than ignore it. Silently
	// dropping it produces a machine that comes up, looks healthy to the backend,
	// and never joins anything -- a failure that only surfaces much later, as a
	// Machine that never gets a Node.
	BootstrapData []byte
}

// MachineState is what a backend reports back about a machine.
type MachineState struct {
	// ID is the backend-native identifier, e.g. a libvirt domain UUID.
	ID string

	// Name is the machine's name on the backend, echoed back so the caller can
	// confirm it found what it expected.
	//
	// This exists for a specific attack: providerID is a user-writable spec field
	// at creation time, so anyone able to create a HydraMachine can point it at an
	// unrelated machine's ID. Format and backend validation do not catch that --
	// only comparing the resolved machine's name against the one this object owns
	// does. Never run a destructive operation without that comparison.
	Name string

	// Ready reports that the machine is running and, as far as the backend can
	// tell, usable. It does not mean the machine has joined a cluster -- that is
	// Kubernetes' judgement, not the backend's.
	Ready bool

	Addresses []Address
}

// MachineProvider is implemented by each infrastructure backend.
//
// Every method takes a context because the backend may be remote. Per
// architectural principle 9, Hydra must not assume the controller and the
// hypervisor share a host: they may be a WAN hop apart, in another building.
// Implementations must therefore honour cancellation, and must be safe to retry
// after a failure that may or may not have taken effect.
type MachineProvider interface {
	// Name identifies the backend, and becomes the middle segment of the
	// providerID: hydra://<name>/<id>.
	Name() string

	// Create makes a machine, and MUST be idempotent on spec.Name.
	//
	// This is not a nicety. A reconcile can be interrupted between creating a
	// machine and persisting its providerID, and the next reconcile will call
	// Create again with the same spec. If Create were not idempotent, that window
	// would produce a second machine that nothing owns and nothing cleans up --
	// which is precisely the "exactly one VM per HydraMachine" guarantee failing
	// in the least visible way possible.
	//
	// Implementations must therefore look for an existing machine by name first,
	// and return it rather than creating another.
	Create(ctx context.Context, spec MachineSpec) (*MachineState, error)

	// Get returns the machine's current state, or ErrNotFound.
	Get(ctx context.Context, id string) (*MachineState, error)

	// FindByName resolves the idempotency key back to a machine, or ErrNotFound.
	//
	// This exists for one specific crash window: Create can succeed and the
	// providerID patch that records its ID can then fail. Deletion would
	// otherwise find no ID, skip backend cleanup, release the finalizer, and
	// orphan a running machine permanently. Given only the name, the controller
	// can still find and remove it.
	FindByName(ctx context.Context, name string) (*MachineState, error)

	// Delete removes the machine and everything the backend created for it.
	//
	// Deleting a machine that does not exist MUST succeed. Teardown is retried,
	// and the second attempt finding nothing is success, not failure.
	Delete(ctx context.Context, id string) error

	// DeleteByName removes the machine and any partial resources keyed by the
	// idempotency name -- including a clone volume whose domain was never
	// defined. Create can crash after allocating the volume and before defining
	// the domain; FindByName would then report not-found and deletion would
	// release the finalizer, permanently leaving the qcow2 behind.
	//
	// As with Delete, removing nothing succeeds.
	DeleteByName(ctx context.Context, name string) error
}

// ProviderID renders the Cluster API providerID for a machine.
//
// The Kubernetes Node reports the same string, which is how Cluster API pairs a
// Machine with its Node. Its shape is therefore load-bearing and must stay
// stable across releases.
func ProviderID(backend, id string) string {
	return fmt.Sprintf("hydra://%s/%s", backend, id)
}

// ParseProviderID extracts the backend name and machine ID from a providerID.
func ParseProviderID(providerID string) (backend, id string, err error) {
	const prefix = "hydra://"
	if len(providerID) <= len(prefix) || providerID[:len(prefix)] != prefix {
		return "", "", fmt.Errorf("providerID %q is not a Hydra provider ID", providerID)
	}
	rest := providerID[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			if i == 0 || i == len(rest)-1 {
				break
			}
			return rest[:i], rest[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("providerID %q is malformed, want hydra://<backend>/<id>", providerID)
}
