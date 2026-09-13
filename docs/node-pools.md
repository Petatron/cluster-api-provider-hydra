# Node pools

A Hydra node pool is a group of interchangeable machines an operator declares by
*shape and size* rather than by name. Nothing in it is new: a pool is

```
MachineDeployment  +  HydraMachineTemplate  +  KubeadmConfigTemplate
```

and that is deliberate. Hydra adds **no node-pool CRD and no second controller**.
Cluster API already reconciles desired replica counts, performs rolling
replacement, and integrates with Cluster Autoscaler; a Hydra-owned layer on top
would duplicate all three and then have to be kept in agreement with them. "Node
pool" is a word for a shape people already use, not an object.

Each of the three objects owns one question:

| object | answers |
| --- | --- |
| `HydraMachineTemplate` | what a machine in this pool *is* — vCPUs, memory, disk, image, networks |
| `KubeadmConfigTemplate` | what the node *becomes* on join — labels, taints, kubelet config |
| `MachineDeployment` | how many, at what version, and within which bounds |

## Where each field goes

This table is the substance of the model, and most of it is not guessable — it
was established by reading what actually consumes each field.

| concern | goes on | notes |
| --- | --- | --- |
| vCPUs, memory, disk, image, networks | `HydraMachineTemplate.spec.template.spec` | immutable; see [Rollout](#rollout) |
| capacity for scale-from-zero | `HydraMachineTemplate.status` | written by the provider, not by you — see [scale-from-zero.md](./scale-from-zero.md) |
| replicas, version | `MachineDeployment.spec` | |
| failure-domain intent | nowhere useful yet — see below | |
| autoscaler bounds | `MachineDeployment` annotations `cluster.x-k8s.io/cluster-api-autoscaler-node-group-{min,max}-size` | a pool without both is not a node group at all |
| **node labels (real)** | `KubeadmConfigTemplate` → `joinConfiguration.nodeRegistration.kubeletExtraArgs` `node-labels` | the only unrestricted path to a Node |
| **node labels (autoscaler's model)** | `MachineDeployment` annotation `capacity.cluster-autoscaler.kubernetes.io/labels` | set the same values in both places |
| **node taints (real)** | `KubeadmConfigTemplate` → `joinConfiguration.nodeRegistration.taints` | see the trap below |
| **node taints (autoscaler's model)** | `MachineDeployment` annotation `capacity.cluster-autoscaler.kubernetes.io/taints` | set the same values in both places |
| capacity overrides | `MachineDeployment` `capacity.cluster-autoscaler.kubernetes.io/*` annotations | override provider-published capacity |

Labels and taints appear **twice** on purpose. One copy configures the real
node; the other configures the hypothetical node Cluster Autoscaler simulates
when deciding whether a scale-up would help. They are read by different
components from different places, and neither derives the other, so keeping them
in agreement is the operator's job. A pool whose two copies disagree autoscales
against a cluster that does not exist.

## Three traps, all of which admit cleanly and then do nothing

Each was confirmed against the running management cluster rather than inferred.

### `spec.template.spec.taints` on a MachineDeployment does not taint anything

The field exists in the v1beta2 schema and the API server accepts it. Whether it
reaches a Node depends on a Cluster API feature gate, and on this management
cluster it is **off**:

```
--feature-gates=...,MachineTaintPropagation=false
```

Cluster Autoscaler reads that same field for its simulated node **regardless of
the gate**. So filling it in produces the worst available outcome: the
autoscaler believes the pool's nodes are tainted, and they are not. Pods that
should have been kept off the pool land on it, and scale-up decisions are made
against a taint nothing enforces.

Put real taints in the `KubeadmConfigTemplate`, where kubeadm applies them at
registration and no gate is involved.

### Most labels never reach the Node

`MachineDeployment.spec.template.metadata.labels` lands on the **Machine**.
Cluster API propagates a Machine label onward to its Node only when it matches
one of three managed domains:

```
node-role.kubernetes.io          node-restriction.kubernetes.io          node.cluster.x-k8s.io
```

So `hydra.petatron.io/pool: compute` set there reaches the Machine and stops.
A `nodeSelector` written against it matches nothing, and the pods wait forever.
Either use a label inside one of those domains, or set it through
`node-labels` in the `KubeadmConfigTemplate` — which the kubelet applies itself
and which has no domain restriction.

The empty state is easy to mistake for a bug. A worker built before this
document had exactly the kubelet's own defaults on it:

```
beta.kubernetes.io/arch, beta.kubernetes.io/os,
kubernetes.io/arch, kubernetes.io/hostname, kubernetes.io/os
```

No pool identity at all — because nothing had ever put one there.

### `spec.template.spec.metadata` does not exist

Cluster Autoscaler looks for pool labels at
`spec.template.spec.metadata.labels`. That path is not part of the v1beta2
`MachineDeployment` schema:

```
$ kubectl explain machinedeployment.spec.template.spec.metadata
error: field "metadata" does not exist
```

So the autoscaler finds nothing there no matter what you write, and the
`capacity.cluster-autoscaler.kubernetes.io/labels` annotation is the only way to
tell it about a pool's labels.

### Failure-domain intent has nowhere to go

PET-28 asked for it, and the honest answer is that Hydra has no contract for it
yet.

`failureDomain` is a `Machine` field, so on a pool it would be
`MachineDeployment.spec.template.spec.failureDomain` — not
`MachineDeployment.spec.failureDomain`, which does not exist. More to the point,
the provider never reads it. `HydraMachine.status.failureDomain` is *reporting*
only ("where the backend put it", which is not necessarily where you asked), and
nothing in the controller writes even that outside one test that hard-codes
`workstation`.

So: one hypervisor today, the field is unused, and setting it buys no spread.
Leave it unset rather than expressing an intent nothing acts on.

## Rollout

A MachineDeployment rolls when **its own `spec.template` changes**. Editing an
object it *references* is not a change to it, so Cluster API does not notice —
which splits pool edits into three kinds:

| change | what to do | rolls? |
| --- | --- | --- |
| replicas, min/max bounds | edit in place | no, and none is wanted |
| machine shape — vcpus, memory, disk, image, networks | **new `HydraMachineTemplate` name**, repoint `infrastructureRef` | yes |
| labels, taints, kubelet config | **new `KubeadmConfigTemplate` name**, repoint `bootstrap.configRef` | yes |

`HydraMachineTemplate.spec` is immutable in its entirety, so the API forces the
second row on you. Nothing forces the third, and that is the trap: editing a
`KubeadmConfigTemplate` in place is accepted, changes nothing about existing
machines, and applies only to machines created afterwards. The pool ends up
running two generations of node with different labels and no indication that it
does.

It is worse than a no-op when labels or taints are involved, because the
autoscaler's copy of them lives on the MachineDeployment's annotations. Updating
that annotation takes effect for the whole group immediately, while every
running node keeps whatever kubeadm gave it. That is precisely the
"autoscales against a cluster that does not exist" failure the traps above are
trying to prevent — reached by editing one field.

Both template kinds therefore carry their content in the name
(`pool-compute-4c8g`), so the next one is easy to name and the old one stays an
accurate record of what its machines were built from.

## The three pools

Valid, distinct examples live in [`examples/`](./examples):

| pool | bounds | shape | why it differs |
| --- | --- | --- | --- |
| [`nodepool-system.yaml`](./examples/nodepool-system.yaml) | min **1**, max 3 | 2 vCPU / 4Gi / 40Gi | runs cluster add-ons, so it must never reach zero — there would be nothing left to schedule the thing that scales it back up. Tainted, which **repels but does not attract** — see below |
| [`nodepool-compute.yaml`](./examples/nodepool-compute.yaml) | min **0**, max 6 | 4 vCPU / 8Gi / 80Gi | ordinary workloads; the pool that demonstrates scale-from-zero |
| [`nodepool-gpu.yaml`](./examples/nodepool-gpu.yaml) | min **0**, max 2 | 8 vCPU / 32Gi / 200Gi | tainted for GPU work; capacity advertised by annotation — see below |

### A taint repels; it does not pin anything to the pool

The system pool's `CriticalAddonsOnly=true:NoSchedule` keeps *other* work off it.
It does nothing to bring add-ons *onto* it — that needs the workload to ask, and
most will not:

* kube-proxy and Cilium are DaemonSets tolerating `operator: Exists`, so they
  run on every node including this one. They would anyway.
* kubeadm's CoreDNS **does** tolerate `CriticalAddonsOnly` (`operator: Exists`),
  so the taint does not exclude it — but tolerating is not preferring, and
  nothing stops it landing on the compute pool instead.

A pool that is only tainted is therefore a pool that may run nothing, while
still counting against `min: 1`. Workloads that belong there need the pair:

```yaml
tolerations:
  - key: CriticalAddonsOnly
    operator: Equal
    value: "true"
    effect: NoSchedule
nodeSelector:
  hydra.petatron.io/pool: system
```

The `nodeSelector` is the half that actually places the pod, and it only works
because the label reaches the Node through kubeadm — see the label trap above.

### The GPU pool is shaped, not yet real

`HydraMachineSpec` has no GPU field, because PET-33 has not decided whether
libvirt exposes a GPU by VFIO passthrough or by bare metal. The example
therefore declares the *pool* — bounds, labels, taint, machine size — and
advertises the GPU itself through the autoscaler's
`capacity.cluster-autoscaler.kubernetes.io/gpu-type` and `/gpu-count`
annotations, which need no provider support at all.

That is enough for the autoscaler to reason about a min=0 GPU pool today. What
it does **not** do is attach a GPU to the VM: applying this example produces
machines with the declared size and no device.

**So do not apply it next to a workload that requests `nvidia.com/gpu`.** The
autoscaler would see the pod fit its simulated node, scale up, get an 8 vCPU /
32Gi machine with no device, leave the pod `Pending`, and repeat until `max`.
That burns both of the large VMs the small max is there to cap, and it presents
as a broken autoscaler rather than a missing feature. The max is deliberately 2
for the same reason.

It is a complete description of the pool's scheduling behaviour and an
incomplete description of its hardware, and it stays that way until PET-33.

## What is verified, and what is not

The objects, the field placement and the three traps were checked against the
live management cluster — schema, feature gates, propagation domains, and the
labels actually present on a running worker.

The examples pass server-side dry-run, and their bootstrap is the join recipe
proven on this hardware in PET-37 and PET-17 — copied whole rather than
summarised, because a partial bootstrap that looks complete builds machines that
boot, look healthy, and never become Nodes. **Dry-run cannot catch that**: the
bar for a pool example is joinability, not admission, and admission is all a
dry-run tests.

They **have not been applied**, since doing so creates virtual machines. So the
field placement, the traps and the recipe are each verified, while the assembled
result is not. A zero-replica pool is the cheap way to change that — it builds
nothing until a pod pends — and that is PET-11's policy call and PET-12's
demonstration.
