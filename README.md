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

> **Status: scaffold.** The project builds, generates, and tests, but the API types are empty
> placeholders and the controller does nothing yet. See [Roadmap](#roadmap).

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
├── HydraMachine          — one infrastructure machine, backing one CAPI Machine
└── HydraMachineTemplate  — the template a MachineDeployment stamps machines from
```

`HydraCluster` is planned but not yet scaffolded.

### HydraMachine

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: HydraMachine
spec:
  vcpus: 2
  memory: 4Gi          # Kubernetes quantity
  diskSize: 40Gi       # Kubernetes quantity
  image:
    name: ubuntu-24.04 # resolved by the backend; url + checksum optional
  networks:
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
  worse than making them state it, so `vcpus`, `memory`, `diskSize`, `image` and
  `networks` are all required.
- **No admission webhook.** Everything above is declarative, which keeps
  cert-manager out of the install path. Value checks that CEL cannot express
  cleanly against `Quantity` are left to the controller (PET-8).

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

### Building the clusterctl release manifest

```bash
make release-manifests IMG=<registry>/cluster-api-provider-hydra:tag
```

Writes `out/infrastructure-components.yaml` and `out/metadata.yaml` — the two files `clusterctl`
needs to install this provider. `metadata.yaml` declares which Cluster API contract each release
series implements; keep it accurate or `clusterctl` will refuse the provider.

## Repository layout

```
api/v1alpha1/          API types (HydraMachine, HydraMachineTemplate)
cmd/main.go            manager entrypoint; registers both this provider's and CAPI's schemes
internal/controller/   reconcilers
config/                kustomize manifests — CRDs, RBAC, manager Deployment
  └── default/         adds the cluster.x-k8s.io/provider label clusterctl keys off
test/                  envtest and e2e helpers
metadata.yaml          clusterctl contract mapping
```

Backend implementations (libvirt first, others later) will live under
`internal/providers/<backend>/`, behind a `MachineProvider` interface, so that backend specifics do
not leak into the API types.

## Roadmap

| Issue | Work |
|---|---|
| PET-6 | Scaffold — *this* |
| PET-7 | Define the `HydraMachine` / `HydraMachineTemplate` API contracts |
| PET-8 | Implement the libvirt machine backend and reconciliation |
| PET-9 | Integrate CABPK bootstrap data; prove `MachineDeployment` scale-out |
| PET-27 | `InfraMachineTemplate` capacity contract for autoscaler scale-from-zero |
| PET-30 | Controller and provisioning-lifecycle metrics |

## License

Apache 2.0 — see [LICENSE](./LICENSE).
