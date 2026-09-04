# cluster-api-provider-hydra

[![Tests](https://github.com/Petatron/cluster-api-provider-hydra/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/Petatron/cluster-api-provider-hydra/actions/workflows/test.yml)
[![Lint](https://github.com/Petatron/cluster-api-provider-hydra/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/Petatron/cluster-api-provider-hydra/actions/workflows/lint.yml)
[![Verify](https://github.com/Petatron/cluster-api-provider-hydra/actions/workflows/verify.yml/badge.svg?branch=main)](https://github.com/Petatron/cluster-api-provider-hydra/actions/workflows/verify.yml)
[![Coverage](https://codecov.io/gh/Petatron/cluster-api-provider-hydra/branch/main/graph/badge.svg)](https://codecov.io/gh/Petatron/cluster-api-provider-hydra)
[![Cluster API](https://img.shields.io/badge/Cluster%20API-v1.14%20%C2%B7%20contract%20v1beta2-326CE5?logo=kubernetes&logoColor=white)](https://cluster-api.sigs.k8s.io/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](./LICENSE)

A [Cluster API](https://cluster-api.sigs.k8s.io/) **infrastructure provider** for Hydra. It
reconciles Cluster API `Machine` objects into real machines on an on-prem hypervisor, starting with
libvirt/KVM.

> **Status: early, but running.** The API types are defined, the libvirt backend provisions
> machines from CABPK bootstrap data, and the provider has been deployed against a real hypervisor
> — a `HydraCluster` verifies there and its `Cluster` reports `InfrastructureReady`. What has not
> been proven end to end yet is `MachineDeployment` scale-out on that hardware. See
> [Roadmap](#roadmap).

Design documents live in Notion under **Project Hydra**; execution is tracked in Linear under the
`PET` project.

## What this provider is responsible for

Cluster API splits cluster lifecycle across several providers. This one owns only the
**infrastructure** half for Hydra:

| Concern | Owner |
|---|---|
| `Cluster` / `Machine` / `MachineDeployment` lifecycle | Cluster API core |
| Turning a `Machine` into a bootable VM | **this provider** |
| Generating bootstrap data (cloud-init) | Kubeadm Bootstrap Provider (CABPK) |
| Control-plane lifecycle | Kubeadm Control Plane Provider (KCP) |
| Deciding how many machines are needed | Cluster Autoscaler |

The deliberate consequence: this provider does **not** implement its own machine lifecycle,
scaling logic, or bootstrap mechanism. It implements the Cluster API infrastructure contract and
nothing more.

## API

```
infrastructure.cluster.x-k8s.io/v1alpha1
├── HydraCluster          — one cluster's infrastructure, backing one CAPI Cluster
├── HydraMachine          — one infrastructure machine, backing one CAPI Machine
└── HydraMachineTemplate  — the template a MachineDeployment stamps machines from
```

### HydraCluster

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: HydraCluster
spec:
  controlPlaneEndpoint:      # supplied, not produced — see below
    host: 192.168.16.10
    port: 6443
  storagePool: k8s-workers   # where machine disks are created, and where the base image lives
  baseImage:                 # inherited by machines that name no image
    name: ubuntu-24.04-server-cloudimg-amd64.img
  networks:                  # inherited by machines that name no networks
    - name: br0
status:
  initialization:
    provisioned: true        # contract: gates machine creation for the whole cluster
  conditions: []             # a "Ready" condition mirrors to Cluster.InfrastructureReady
```

A `Cluster` points at one of these through `spec.infrastructureRef`, which is what
makes it admissible at all — Cluster API rejects a `Cluster` carrying none of
`infrastructureRef`, `controlPlaneRef` or `topology`.

Meeting that contract is the smaller half of the type's job. `storagePool`,
`baseImage` and `networks` are the cluster-wide settings machines inherit when
they omit their own, resolving machine → cluster → manager flag. They were
process-wide manager flags before, which meant one pool and one default image for
every cluster a single controller managed; carrying them per cluster is the point
of the object. `storagePool` is fixed at creation including whether it is set at
all, since omitting it already selects the manager's pool — allowing it to be
added later would leave existing machines in one pool and every later machine in
another.

`controlPlaneEndpoint` is supplied by whoever writes the object rather than
produced by the provider. Providers that create the endpoint themselves — with a
load balancer or a VIP — are the ones expected to fill it in; Hydra creates
neither, so it only reports back what it is told. Immutable once set: moving a
live cluster's API endpoint is not something a reconcile can carry out.

Its controller **verifies rather than creates**. There is no network to build and
no load balancer to stand up, so what it does is confirm that the infrastructure
the cluster was pointed at actually exists — the storage pool is running, and the
base image is present in it — and report that through
`status.initialization.provisioned`, which gates machine creation. Both absences
fail every machine in the cluster identically, so checking once at cluster level
turns a late, repeated, per-machine confusion into a single condition raised
before anything is attempted.

Deliberately **not** here: how to reach the hypervisor. That is still a manager
flag, and moving it onto this object is PET-38, bundled with the switch to TLS.
It is left out rather than stubbed because a field the provider accepts and
ignores is worse than one that does not exist.

### HydraMachine

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: HydraMachine
spec:
  vcpus: 2
  memory: 4Gi          # Kubernetes quantity
  diskSize: 40Gi       # Kubernetes quantity
  image:               # optional; the cluster's baseImage is inherited when omitted
    name: ubuntu-24.04 # resolved by the backend; url + checksum optional
  networks:            # optional; the cluster's networks are inherited when omitted
    - name: br0        # backend network or bridge
  # providerID is written by the controller, not by you
status:
  initialization:
    provisioned: true  # contract: drives Cluster API provisioning orchestration
  addresses:           # surfaced on the Machine once provisioned
    - type: InternalIP
      address: 192.168.15.42
  failureDomain: workstation  # where the backend actually placed it
  conditions: []       # a "Ready" condition mirrors to Machine.InfrastructureReady
```

Applied on its own — with no Cluster API `Machine` to own it — a `HydraMachine`
also needs `hydramachine.infrastructure.cluster.x-k8s.io/standalone: ""`. The
controller will not infer that from a missing owner reference: a machine Cluster
API is about to adopt looks identical until the reference is stamped on, and
provisioning in that window builds a VM with no bootstrap data and the wrong
defaults. `config/samples/` carries a complete, applyable example.

Every spec field is **backend-neutral**. A machine is described by what it must
provide — capacity, an image, network attachments — never by how a particular
backend provides it. There is no libvirt vocabulary in the API and no untyped
escape hatch; backend-specific configuration belongs to the backend. That is what
keeps a future Proxmox or bare-metal backend from being bolted onto an API shaped
around libvirt.

`memory` and `diskSize` are `resource.Quantity` rather than integers-with-units.
That reads naturally (`8Gi`), and it is the same type `corev1.ResourceList` uses,
so the autoscaler capacity contract (PET-27) can reuse these values directly.

Deliberately **not** here: GPU. That belongs to the machine-class model in PET-28,
and adding an optional field later is a non-breaking change.

### Immutability

Capacity, image and networks are immutable, enforced by CEL rules in the CRD.
Cluster API's model for changing a machine is to roll it out and replace it, so a
silently-ignored edit becomes an immediate rejection instead. `providerID` is
write-once: absent until the controller sets it, frozen afterwards.

For `image` and `networks` that covers whether they are set at all, not only what
they are set to. Adding one to an existing machine would otherwise be admitted
while changing nothing: the VM was built from whatever the field resolved to at
creation, and the reconciler stops re-reading a machine once its `providerID`
exists — so the object would claim an image the running VM does not have.

`HydraMachineTemplate.spec` is immutable in its entirety — changing a machine
shape means creating a new template and rolling the MachineDeployment onto it,
which keeps a template an accurate record of what its machines were built from.
Setting `providerID` inside a template is rejected outright: it identifies one
machine, and a template describes many.

> **Known limitation.** Immutability is enforced with CEL rather than an admission
> webhook. The Cluster API contract asks providers to skip template immutability
> checks during the topology controller's server-side-apply dry runs, which needs
> a webhook to distinguish. Hydra does not use ClusterClass/topology yet — it is
> listed under future enhancements — so CEL is sufficient today. Adopting
> ClusterClass will require revisiting this.

### Validation and defaulting strategy

- **Structural validation** (required fields, min/max, patterns, list keys) comes
  from kubebuilder markers and is enforced by the API server.
- **Cross-field and transition rules** use CEL `XValidation` — image requires
  name or url; immutability; no `providerID` in templates.
- **No defaulting of capacity.** Quietly choosing a machine size for someone is
  worse than making them state it, so `vcpus`, `memory` and `diskSize` are
  required on every machine. `image` and `networks` are optional only because a
  `HydraCluster` can supply them; a machine that resolves to neither is still
  refused — at reconcile time, where the cluster is readable, rather than at
  admission.
- **No admission webhook.** Everything above is declarative, which keeps
  cert-manager out of the install path. Even the `Quantity` comparisons are CEL,
  through its `quantity()` library: memory and disk must exceed zero, and since
  both fields are immutable, a machine admitted with a zero could never be
  corrected afterwards — only deleted.

All of this is tested against a real API server via envtest — CEL rules do not
live in Go, so a unit test on the types would prove nothing. The shipped samples
are applied in the same suite, so schema and samples cannot drift apart.

## Compatibility

| | Version |
|---|---|
| Cluster API | v1.14.0 |
| Cluster API contract | `v1beta2` |
| Go | 1.26 |
| Kubebuilder | v4.15.0 |
| controller-runtime | v0.24.1 |

**Note on imports.** As of Cluster API v1.14 the API types live in a *separate Go module*,
`sigs.k8s.io/cluster-api/api`, and the core package path is
`sigs.k8s.io/cluster-api/api/core/v1beta2`. Older guides referencing
`sigs.k8s.io/cluster-api/api/v1beta1` inside the main module are out of date and will not resolve.

## Local development

Requirements: Go 1.26 (matching `go.mod`, the `Dockerfile`, and the devcontainer), `make`, Docker (for envtest and image builds). Everything else —
`controller-gen`, `kustomize`, `setup-envtest`, envtest binaries — is downloaded into `bin/` on
first use.

```bash
make generate          # deepcopy / runtime.Object methods
make manifests         # CRDs and RBAC from // +kubebuilder markers
make build             # binary at bin/manager
make test              # unit tests against a real API server via envtest
make lint              # golangci-lint
```

After changing anything under `api/`, run `make generate manifests` — the generated code and CRDs
are committed, so a diff there means you forgot.

`make test` writes a coverage profile to `cover.out`; `go tool cover -html=cover.out` renders it.
CI uploads the same file to Codecov, which is where the coverage badge comes from. What counts
towards it is configured in [`codecov.yml`](./codecov.yml).

### Running the controller against a cluster

```bash
make install                       # apply CRDs to the current kubectl context
make run                           # run the manager locally, outside the cluster
```

`make run` uses your kubeconfig, so the controller runs on your machine while watching the remote
cluster. That is the fastest edit/test loop; no image build, no redeploy.

### Deploying into a cluster

```bash
make docker-build docker-push IMG=<registry>/cluster-api-provider-hydra:tag
make deploy IMG=<registry>/cluster-api-provider-hydra:tag
```

The manager still needs a path to libvirtd, and that is per-host configuration the manifests cannot
guess. [`config/manager/manager.yaml`](./config/manager/manager.yaml) carries the remote and
local-socket variants side by side, including the two settings the local socket also needs — the
hypervisor's `libvirt` gid, and a uid libvirtd can resolve to a host user record. Both were found
by deploying it, and both surface as a connection failure rather than a permission one — which sends
you looking at the socket path or at libvirtd itself.

### Building the clusterctl release manifest

```bash
make release-manifests IMG=<registry>/cluster-api-provider-hydra:tag
```

Writes `out/infrastructure-components.yaml` and `out/metadata.yaml` — the two files `clusterctl`
needs to install this provider. `metadata.yaml` declares which Cluster API contract each release
series implements; keep it accurate or `clusterctl` will refuse the provider.

## Repository layout

```
api/v1alpha1/          API types (HydraCluster, HydraMachine, HydraMachineTemplate)
cmd/main.go            manager entrypoint; registers both this provider's and CAPI's schemes
internal/controller/   reconcilers
internal/providers/    the backend-neutral MachineProvider interface, and libvirt behind it
internal/cloudinit/    the NoCloud image that carries CABPK bootstrap data into guests
config/                kustomize manifests — CRDs, RBAC, manager Deployment
  └── default/         adds the cluster.x-k8s.io/provider label clusterctl keys off
test/                  envtest and e2e helpers
metadata.yaml          clusterctl contract mapping
```

Backend implementations live under `internal/providers/<backend>/`, behind the `MachineProvider`
interface, so that backend specifics do not leak into the API types. libvirt is the only one so
far; nothing above the interface names it.

## Roadmap

| Issue | Work | Status |
|---|---|---|
| PET-6 | Scaffold | shipped |
| PET-7 | Define the `HydraMachine` / `HydraMachineTemplate` API contracts | shipped |
| PET-8 | Implement the libvirt machine backend and reconciliation | shipped |
| PET-9 | Integrate CABPK bootstrap data, delivered to guests as a NoCloud image | shipped |
| PET-15 | Define `HydraCluster` and the cluster-wide defaults machines inherit | shipped |
| PET-37 | Prove `MachineDeployment` scale-out on real hardware | in progress |
| PET-38 | Move the hypervisor connection onto `HydraCluster`, with TLS | planned |
| PET-27 | `InfraMachineTemplate` capacity contract for autoscaler scale-from-zero | planned |
| PET-30 | Controller and provisioning-lifecycle metrics | planned |

## License

Apache 2.0 — see [LICENSE](./LICENSE).
