# cluster-api-provider-hydra

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

## API group

```
infrastructure.cluster.x-k8s.io/v1alpha1
├── HydraMachine          — one infrastructure machine, backing one CAPI Machine
└── HydraMachineTemplate  — the template a MachineDeployment stamps machines from
```

`HydraCluster` is planned but not yet scaffolded.

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
