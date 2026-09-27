# Node-pool autoscaling policy

Two annotations decide how far a pool may grow and shrink:

```yaml
metadata:
  annotations:
    cluster.x-k8s.io/cluster-api-autoscaler-node-group-min-size: "0"
    cluster.x-k8s.io/cluster-api-autoscaler-node-group-max-size: "6"
```

They belong on the **`MachineDeployment`** (or `MachineSet`) — the scalable
resource — never on the `HydraMachineTemplate`.

This is **policy**: how many machines an operator is willing to pay for. It is a
separate question from **capacity discovery**, which is how the autoscaler learns
what one machine in the pool would look like, and which is
[`scale-from-zero.md`](scale-from-zero.md). Everything below is independent of it
except one thing, and that one thing is sharper than "needs the capacity
contract":

**A pool sitting at `replicas: 0` with a broken capacity contract is not a node
group at all — silently.** The check runs before the `max == 0` gate, though
*after* the bounds have been parsed and validated — so this silence assumes the
annotations themselves parse cleanly. A pool with both a broken capacity contract
and a malformed bound fails loudly instead:

```go
// clusterapi_nodegroup.go, newNodeGroupFromScalableResource
if found && replicas == 0 && !scalableResource.CanScaleFromZero() {
    return nil, nil
}
```

and `CanScaleFromZero()` is only "does the resolved capacity contain both `cpu`
and `memory`". It returns `nil, nil` — no error, no log line, not even at
`V(4)`. The pool never appears in `NodeGroups()` and does not come back until
somebody scales it to 1 by hand.

That matters most when validating a new pool: if the capacity contract is wrong,
the symptom is **not** a discovered pool that declines to grow. It is silence,
indistinguishable from the autoscaler never having noticed the apply.

So the first thing to check is `HydraMachineTemplate.status.capacity`, not the
annotations — but for a reason specific to these examples rather than a general
one. `InstanceCapacity()` *does* fall back to the capacity annotations when the
infrastructure object cannot be read, but only `if len(capacityAnnotations) > 1`,
and `pods` is populated unconditionally — so the fallback needs a real
`capacity.cluster-autoscaler.kubernetes.io/cpu` or `/memory`. All three examples
set only `/labels` and `/taints`, so there is no fallback and the template status
is the only source.

## What the two annotations actually gate

The natural reading is that both are required and a pool missing either is
ignored. That is not what happens. One rule covers every case:

> **A missing annotation reads as `0`. Then two gates run, in order:
> `max < min` is a hard error, and `max == 0` is a silent skip.**

A missing annotation is tolerated, not rejected — `parseScalingBounds`
propagates an error only for a malformed value, and the "missing" error is
swallowed after the bound has already defaulted to `0`:

```go
// clusterapi_utils.go
minSize, err := minSize(annotations)
if err != nil && err != errMissingMinAnnotation {
    return 0, 0, err
}
...
if maxSize < minSize {
    return 0, 0, errInvalidMaxAnnotation   // gate 1: hard error
}
```

```go
// clusterapi_nodegroup.go
if scalableResource.MaxSize()-scalableResource.MinSize() < 0 || scalableResource.MaxSize() == 0 {
    klog.V(4).Infof("nodegroup %s has no scaling capacity, skipping", scalableResource.Name())
    return nil, nil                        // gate 2: silent skip
}
```

Everything follows from that:

| annotations | effective (min, max) | result |
| --- | --- | --- |
| `min "0"`, `max "6"` | (0, 6) | discovered |
| `max "6"` only | (0, 6) | **discovered, `min` silently `0`** |
| `min "0"` only | (0, 0) | skipped |
| `min "1"` only | (1, 0) | **hard error — `max < min`** |
| `max "0"`, min absent or `"0"` | (0, 0) | skipped |
| `max "0"`, `min "1"` | (1, 0) | **hard error — `max < min`** |
| non-integer or negative | — | hard error |

Two consequences, and they pull in opposite directions.

**Dropping `min-size` fails open.** It does not get you a pool the autoscaler
ignores; it gets you a fully discovered pool with a floor of zero. With
scale-down on — as it has been on `hydra-wl0` since PET-13 — that is a licence to
drain the system pool's add-on nodes the moment they look idle.

**Dropping or zeroing `max-size` fails loudly — but only on a pool whose `min` is
above zero, and not locally.** `nodeGroups()` returns `nil, err` on the first
failure, so that one `MachineDeployment` takes out discovery for **every** pool
in the cluster. The same edit on a `min-size: "0"` pool merely removes that pool,
quietly. Of the three examples only
[`nodepool-system.yaml`](examples/nodepool-system.yaml) has a nonzero min, which
makes it the only one where this distinction is live.

## The part that surprises people

**`min-size` is a floor for scale-down, not for scale-up.**

The autoscaler will not shrink a group below `min-size`. It will *not*, by
default, grow a group that is already below it. That is a separate flag,
verified against the v1.35.2 source:

| flag | default | help |
| --- | --- | --- |
| `--enforce-node-group-min-size` | `false` | "Should CA scale up the node group to the configured min size if needed." |

So on a default deployment, `min-size: "1"` does not guarantee one machine. It
guarantees the autoscaler will not be the thing that removes the last one.

**On `hydra-wl0`, `min-size` has been load-bearing since PET-13 turned
scale-down on.** Before that the deployment ran `--scale-down-enabled=false`, and
since scale-down is the only behaviour `min-size` bounds, the annotation
constrained nothing at all. Now it is the floor the autoscaler will not shrink a
pool below. `--enforce-node-group-min-size` is still not set, so it is *only* a
scale-down floor: nothing grows a pool back up to it.

The practical consequence: **a pool's actual floor is its `replicas`, not its
`min-size`.** The system pool in
[`examples/nodepool-system.yaml`](examples/nodepool-system.yaml) carries
`min-size: "1"` *and* `replicas: 1`. The replicas hold the floor; the annotation
records why it must not go lower. Scale that pool to zero by hand and nothing
brings it back — that, and only that, is what `--enforce-node-group-min-size`
would buy.

## Choosing `max-size`

`max-size` is the one bound that is always enforced: the autoscaler will not
scale a group past it, and pods that do not fit stay `Pending`.

Size it against what the hypervisor can actually host, not against what the
workload might want. The failure it prevents is worse than pending pods — a pool
allowed to grow past the host's memory or disk produces machines that are
defined, fail to boot or boot degraded, and take the hypervisor's other guests
down with them. There is no admission check anywhere in Hydra that will catch
this: `MachineDeployment` admits any replica count, and the provider discovers
the shortfall only when libvirt refuses.

Budget across *all* pools on the same hypervisor, including the machines it is
already running that Hydra did not create.

## Choosing `min-size`

Three cases, and the examples show each:

| pool | min | why |
| --- | --- | --- |
| [system](examples/nodepool-system.yaml) | `1` | Runs the cluster's own add-ons. A cluster with none of these nodes cannot schedule the things that make it a cluster, so the pool must never be a candidate for removal. |
| [compute](examples/nodepool-compute.yaml) | `0` | General workload. Nothing needs to exist when nothing is pending; building machines to sit idle is the cost the autoscaler exists to avoid. |
| [gpu](examples/nodepool-gpu.yaml) | `0` | The same argument, more sharply — GPU machines are the most expensive thing to leave idle, and the pool is shaped but not yet real (PET-33). |

A `min-size` above zero is a claim that the pool's absence breaks something. If
it does not, use zero: a min above zero on a pool that could be empty is a
standing bill.

## When to turn on `--enforce-node-group-min-size`

Turn it on when a pool's floor must survive something other than the autoscaler
removing machines — a machine deleted by hand, a `MachineDeployment` scaled to
zero by mistake, a node lost with its machine.

Know what it costs before doing so: it makes the autoscaler build machines
because of a number, with no pending pod anywhere. On a pool whose `min-size`
was set aspirationally rather than deliberately, that is a fleet of idle
machines appearing the moment the flag is added. Audit every pool's `min-size`
first — on this deployment they have never been enforced as a scale-*up* floor,
so in that role they have never been tested.

It is a whole-autoscaler flag, not per-pool. One pool needing a hard floor turns
it on for every pool in the cluster.

**That is true of this flag, not of scale-down policy generally.** The clusterapi
provider reads per-node-group overrides off the scalable resource, under the
prefix `cluster.x-k8s.io/autoscaling-options-`:

```
cluster.x-k8s.io/autoscaling-options-scaledownutilizationthreshold
cluster.x-k8s.io/autoscaling-options-scaledowngpuutilizationthreshold
cluster.x-k8s.io/autoscaling-options-scaledownunneededtime
cluster.x-k8s.io/autoscaling-options-scaledownunreadytime
cluster.x-k8s.io/autoscaling-options-maxnodeprovisiontime
cluster.x-k8s.io/autoscaling-options-maxnodestartuptime
```

They override the cluster-wide defaults for that pool alone, and since PET-13
they are live on `hydra-wl0`. The one in use is the exemption on
`hydra-wl0-md-0`, whose workers run all of Argo CD with no PodDisruptionBudget:

```yaml
cluster.x-k8s.io/autoscaling-options-scaledownutilizationthreshold: "0"
```

It works, with a limit worth knowing. A node counts as underutilised when its
utilisation is **at or below** the threshold — the source tests
`Utilization > threshold` for "not underutilised" — so `"0"` protects every node
that requests anything, but **not** a node with literally nothing requested.
DaemonSets normally keep a node above zero (on md-0 they alone request 20m CPU),
so the exemption holds in practice rather than by construction; `min-size` stays
the absolute floor.

Two failure modes, neither of which raises an error. A value that does not parse
logs `failed to convert autoscaling_options option … to float` at warning level
and the cluster default applies; an **empty** value is skipped with no log at all.
The key is lowercase and the prefix match is case-sensitive. Confirm an exemption
from the log rather than trusting the annotation — see [Checking it](#checking-it).

`scaledownunneededtime` on the GPU pool remains an obvious want once that pool is
real (PET-33).

## How scale-down removes a node

What PET-13 observed on `hydra-wl0`, with the governing defaults read from the
1.35.2 source:

1. **Underutilised.** Utilisation is the higher of cpu and memory *requested*
   over allocatable — a GPU node uses its GPU instead. At or below
   `--scale-down-utilization-threshold` (default `0.5`), or the pool's override,
   the node becomes a candidate.
2. **Simulated.** The autoscaler simulates moving every pod elsewhere. One pod
   that cannot move makes the whole node unremovable: a PodDisruptionBudget with
   no disruptions left, a pod using local storage such as `emptyDir` or `hostPath`
   (`--skip-nodes-with-local-storage` defaults to true), or nowhere to schedule.

   ```
   Node pool-compute-… cannot be removed: not enough pod disruption budget to move default/pdb-demo-…
   ```

   An unremovable node is re-simulated only after
   `--unremovable-node-recheck-timeout` (default 5 minutes), not on every loop.
   PET-13's refusals landed 5 minutes apart. A log sample shorter than that can
   show nothing at all and look like a failure.
3. **Unneeded.** A node that passes the simulation must stay unneeded for
   `--scale-down-unneeded-time` (default 10 minutes, overridable per pool) before
   anything happens. PET-13 measured 10m15s from "unneeded" to cordon.
4. **Cooldown.** For `--scale-down-delay-after-add` (default 10 minutes) after any
   scale-up, nothing is deleted — but steps 1 and 2 still run. So a refusal logged
   during cooldown is evidence about the PodDisruptionBudget, not about safety,
   since nothing would have been deleted anyway. A node that survives *after*
   cooldown is the evidence that counts.
5. **Drained by the autoscaler, then deleted by Cluster API.** The autoscaler
   cordons the node and evicts its pods itself — DaemonSet pods included, since
   `--daemonset-eviction-for-occupied-nodes` defaults to true — and only then
   marks the Machine with `cluster.x-k8s.io/delete-machine` and lowers the
   replica count. The MachineSet deletes exactly the marked Machine, and Cluster
   API runs its normal delete lifecycle on the already-empty node: drain, volume
   detach, infrastructure (the VM), bootstrap, Node. In PET-13 that took four
   seconds.

Marking a Machine is an `update` on it, which RBAC cannot pin by name because
Machine names are generated. How `hydra-wl0` fences that grant is in
`hydra-gitops/clusters/hydra-wl0/management-cluster/cluster-autoscaler-rbac.yaml`.

## The bounds and the replica count disagree at your peril

Nothing validates `min-size <= replicas <= max-size`. The annotations are
strings on an unrelated object; CAPI does not read them, and the autoscaler
treats what it finds as the truth.

The combinations that bite:

- **`replicas` above `max-size`** — scale-up is refused, and scale-down does
  not treat the bound as a target: it removes only nodes it finds underutilised.
  So the pool stays over budget until its nodes happen to go idle.
- **`replicas` below `min-size`, enforcement off** — nothing happens, which is
  the case described above. The floor is fiction.
- **`min-size` equal to `max-size`** — a fixed-size pool the autoscaler will
  neither grow nor shrink. Legitimate, but say so in a comment, because it reads
  like a mistake.
- **`max-size: "0"`** — skipped when `min-size` is `0` or absent, with a `V(4)`
  line as the only trace, indistinguishable at default verbosity from a pool
  with nothing to do. With a nonzero `min-size` it is instead `max < min`, the
  cluster-wide hard error below.
- **`min-size` missing** — not an error and not a skip: the pool is discovered
  with a floor of zero. See above; this is the one that fails open.
- **either value malformed** — `max-size: "six"`, a negative number, or
  `max < min` — aborts discovery for every pool in the cluster, not just this
  one.

## Checking it

Node groups the autoscaler actually found, with their bounds:

```sh
kubectl logs -n cluster-autoscaler deploy/cluster-autoscaler | grep -iE 'node ?group'
```

**The optional space is not a typo.** The two messages that matter are spelled
differently, and the one worth reading is the odd one out:

| message | source |
| --- | --- |
| `discovered node group: %s` | `clusterapi_controller.go` |
| `nodegroup %s has no scaling capacity, skipping` | `clusterapi_nodegroup.go` |
| `Unable to read infrastructure reference, error: %v` | `clusterapi_unstructured.go` |

A `grep 'node group'` matches the first and drops the second — so the operator
asking "why was my pool not discovered" greps away the answer.

Note the namespace: the `hydra-wl0` autoscaler runs in `cluster-autoscaler`, not
`kube-system`.

The third line is why the grep matters for the zero-replica case too. The
capacity contract breaks in two ways and only one of them is truly silent:

- **Template unreadable** — RBAC, wrong name, not created yet.
  `readInfrastructureReferenceResource` logs `Unable to read infrastructure
  reference` on the way out. A trace exists, but `grep 'node ?group'` does not
  match it — the same trap as the `nodegroup`/`node group` split above.
- **Template readable, `status.capacity` missing `cpu` or `memory`** — nothing is
  logged anywhere, by anything.

Both end at the same `return nil, nil` and the pool vanishes either way.

Discovery and capacity resolution are logged at `V(4)`, which the `hydra-wl0`
deployment sets. At default verbosity a pool the autoscaler skipped and a pool
with nothing to do look identical — including a pool skipped for missing RBAC.

Scale-down, node by node:

```sh
kubectl logs -n cluster-autoscaler deploy/cluster-autoscaler \
  | grep -iE 'unremovable|cannot be removed|unneeded|Scale-down: node|failed to convert autoscaling_options'
```

| message | meaning |
| --- | --- |
| `Node %s unremovable: %s requested (…% of allocatable) is above the scale-down utilization threshold` | not a candidate. This is also how a working exemption shows up. The resource named is whichever of cpu and memory is higher, ties going to memory, so `memory requested` is not a failure |
| `Node %s cannot be removed: %s` | a candidate the simulation refused — PodDisruptionBudget, local storage, or nowhere to go |
| `%s was unneeded for %s` | a candidate, with the unneeded clock running |
| `Scale-down: node %s removed with drain` | gone |
| `failed to convert autoscaling_options option … to float` | a per-pool override did not parse; the default applies |

Sample at least five minutes of log: see step 2 above.

The annotations as applied:

```sh
kubectl get machinedeployment -A -o custom-columns=\
'NAMESPACE:.metadata.namespace,NAME:.metadata.name,REPLICAS:.spec.replicas,MIN:.metadata.annotations.cluster\.x-k8s\.io/cluster-api-autoscaler-node-group-min-size,MAX:.metadata.annotations.cluster\.x-k8s\.io/cluster-api-autoscaler-node-group-max-size'
```

## What is verified, and what is not

**Verified from source.** Flag names, defaults and semantics were read from the
Cluster Autoscaler v1.35.2 source, the version `hydra-wl0` runs, rather than from
documentation that might describe another release.

**Verified on hardware**, against [`examples/nodepool-compute.yaml`](examples/nodepool-compute.yaml)
on `hydra-wl0`:

| ticket | what was shown |
| --- | --- |
| PET-11 | applied at `replicas: 0`, discovered as `(min: 0, max: 6, replicas: 0)`, no VM built |
| PET-12 | a pending pod scaled it 0 → 1; Running on a Hydra-built VM in 111 seconds (116 on a repeat) |
| PET-13 | a PodDisruptionBudget blocked its removal with scale-down fully armed; once relaxed, it scaled 1 → 0 and the VM and both volumes were reclaimed |

The deployed flag set was read from the live manifest in `hydra-gitops`:
scale-down on (the deprecated `--scale-down-enabled` flag is simply absent), no
`--enforce-node-group-min-size`, and `hydra-wl0-md-0` exempt as described above.

**Not verified.** The system and GPU pools have never been applied. No pool with
`min-size` above zero has been scaled down to its floor, so the scale-down floor
itself is still untested in practice. `--enforce-node-group-min-size` has never
been turned on.
