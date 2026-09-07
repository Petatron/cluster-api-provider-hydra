# Scaling a node pool from zero

A `MachineDeployment` with zero replicas has no `Node` anywhere for Cluster
Autoscaler to inspect, so it cannot know what a machine in that pool would look
like — and a node group whose shape is unknown is one it refuses to scale up.
The `InfraMachineTemplate` contract exists to answer that: the provider
publishes the hypothetical node's capacity on the template, and the autoscaler
sizes the pool from there.

Hydra publishes it on `HydraMachineTemplate.status`. Nothing needs enabling.

```yaml
status:
  capacity:
    cpu: "4"
    memory: 8Gi
    ephemeral-storage: 60Gi
  nodeInfo:
    architecture: amd64
    operatingSystem: linux
```

`cpu` and `memory` come from `spec.template.spec.vcpus` and `.memory`;
`ephemeral-storage` from `.diskSize`. `nodeInfo` is what the configured backend
builds — libvirt domains here are `x86_64`, and machines configure themselves
through cloud-init, so `amd64`/`linux` are structural facts rather than defaults.

## Setup

Two things are needed, neither of them in this provider.

**1. Min/max annotations on the MachineDeployment.** The autoscaler discovers
node groups by these; without them the pool is not a node group at all.
Capacity is irrelevant until they exist.

```yaml
metadata:
  annotations:
    cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size: "0"
    cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size: "5"
```

**2. RBAC for the autoscaler to read templates.** It reads them with a dynamic
client that lists and watches, not a plain get. `config/rbac/cluster_autoscaler_role.yaml`
ships the `ClusterRole`, deliberately unbound — the autoscaler's ServiceAccount
is not this project's to name:

```sh
kubectl create clusterrolebinding hydra-autoscaler-templates \
  --clusterrole=hydramachinetemplate-autoscaler-role \
  --serviceaccount=<namespace>:<autoscaler service account>
```

Miss this and the autoscaler logs the RBAC failure only at `-v=4`, then skips
the node group. At default verbosity a pool it cannot read and a pool with
nothing to scale look identical.

## Precedence

The autoscaler reads the template's status first, then overwrites it with
anything the `capacity.cluster-autoscaler.kubernetes.io/*` annotations on the
**MachineDeployment or MachineSet** specify. Annotations always win.

| Resource | Annotation | Overrides |
| --- | --- | --- |
| cpu | `capacity.cluster-autoscaler.kubernetes.io/cpu` | `status.capacity.cpu` |
| memory | `capacity.cluster-autoscaler.kubernetes.io/memory` | `status.capacity.memory` |
| ephemeral storage | `capacity.cluster-autoscaler.kubernetes.io/ephemeral-disk` | `status.capacity.ephemeral-storage` |
| max pods | `capacity.cluster-autoscaler.kubernetes.io/maxPods` | nothing — see below |
| GPUs | `capacity.cluster-autoscaler.kubernetes.io/gpu-type` + `/gpu-count` | nothing Hydra publishes yet |
| labels | `capacity.cluster-autoscaler.kubernetes.io/labels` | not readable from a template |
| taints | `capacity.cluster-autoscaler.kubernetes.io/taints` | not readable from a template |

Use an annotation to correct a number Hydra derives from machine sizing but the
node does not actually deliver. The overhead gap below is the usual reason.

## Four things worth knowing before debugging this

### `pods` cannot be published from a template

The autoscaler sets the `pods` entry unconditionally — from its `maxPods`
annotation, or from its own default of 110 — *after* merging the template's
status. A `pods` value in `status.capacity` is therefore always discarded.

Hydra does not publish one. It would be read, thrown away, and look like the
authoritative source of a number it has no effect on. If your pool needs a
`maxPods` other than 110, the annotation is the only lever.

### Labels and taints do not come from the template

The autoscaler resolves both from the **MachineDeployment or MachineSet** — from
`spec.template.spec.metadata.labels` and `spec.template.spec.taints` plus the
matching annotations on that same object. It never looks at the infrastructure
template for either.

The one exception is indirect: `status.nodeInfo` becomes the simulated node's
`kubernetes.io/arch` and `kubernetes.io/os` labels. Everything else belongs on
the MachineDeployment.

### Published capacity is capacity, not allocatable

The autoscaler builds its simulated node with `Allocatable = Capacity`. A real
node reports allocatable *below* capacity, so the simulation models slightly
more schedulable room than the machine will actually offer.

Hydra publishes raw machine sizing, which is what the resulting `Node` reports
as `status.capacity`. Publishing pre-reduced numbers instead would produce a
field matching neither the node's capacity nor its allocatable, and every other
Cluster API provider publishes raw sizing too.

The gap on a kubeadm node with no `kube-reserved` or `system-reserved` — the
default — is the eviction threshold: about 100Mi of memory, and 10% of the
filesystem for ephemeral storage. `ephemeral-storage` is further over, because
the node's figure is the filesystem holding `/var/lib/kubelet` rather than the
raw disk, which loses the partition table, `/boot` and filesystem metadata.

It matters in one case: a pod requesting nearly a whole node cannot fit the node
the autoscaler simulated, so the scale-up happens and does not help. If a pool
runs pods sized that close to the machine, publish corrected figures with the
capacity annotations.

### GPUs are representable, but Hydra publishes none

No field on `HydraMachineSpec` declares a GPU, because how libvirt should expose
one — bare metal or VFIO passthrough — is not yet decided (PET-33). A field the
provider could not fill would be worse than its absence.

A min=0 GPU pool works today through `gpu-type` and `gpu-count` on the
MachineDeployment, with no provider support at all. And
`status.capacity` accepts arbitrary resource names, so when PET-33 lands,
publishing `nvidia.com/gpu` from the template needs no API change.

## Checking it

```sh
kubectl get hydramachinetemplate -o custom-columns=\
'NAME:.metadata.name,CPU:.status.capacity.cpu,MEM:.status.capacity.memory,ARCH:.status.nodeInfo.architecture'
```

An empty `CPU` or `MEM` column means this pool cannot be scaled from zero: the
autoscaler requires both, and skips the node group when either is missing.

Values must be JSON **strings**. The autoscaler reads `status.capacity` with
`unstructured.NestedStringMap`, which fails whole rather than per-key — one
numeric value discards the entire map, silently. The quantity schema would
otherwise admit `cpu: 4`, so a CEL rule on the field rejects that write instead.
`kubectl get -o json` shows the stored form if you ever need to confirm it.
