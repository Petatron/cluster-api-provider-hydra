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
[`scale-from-zero.md`](scale-from-zero.md). The two meet in only one place: a
pool with `min-size: "0"` needs the capacity contract, because at zero replicas
there is no `Node` to inspect and an unknown shape is one the autoscaler refuses
to scale up. Everything else here is independent of it.

Without both annotations the pool is **not a node group at all**. The autoscaler
discovers groups by these; capacity is irrelevant until they exist.

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

**On `hydra-wl0` today, `min-size` constrains nothing whatsoever.** That
deployment sets `--scale-down-enabled=false` (PET-10 deliberately left scale-down
to PET-13), and `--enforce-node-group-min-size` is not set. Scale-down is the
only behaviour `min-size` bounds, and it never runs — so the annotation is a
declaration of intent that becomes load-bearing the day PET-13 turns scale-down
on. It is worth setting correctly now precisely because nothing will complain if
it is wrong.

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
first — on this deployment they have never been enforced, so they have never
been tested.

It is a whole-autoscaler flag, not per-pool. One pool needing a hard floor turns
it on for every pool in the cluster.

## The bounds and the replica count disagree at your peril

Nothing validates `min-size <= replicas <= max-size`. The annotations are
strings on an unrelated object; CAPI does not read them, and the autoscaler
treats what it finds as the truth.

The combinations that bite:

- **`replicas` above `max-size`** — the autoscaler will not scale the group down
  to the bound (that is scale-down, which is off here), and will refuse to scale
  it up. The pool sits over budget indefinitely.
- **`replicas` below `min-size`, enforcement off** — nothing happens, which is
  the case described above. The floor is fiction.
- **`min-size` equal to `max-size`** — a fixed-size pool the autoscaler will
  neither grow nor shrink. Legitimate, but say so in a comment, because it reads
  like a mistake.

## Checking it

Node groups the autoscaler actually found, with their bounds:

```sh
kubectl logs -n kube-system deploy/cluster-autoscaler | grep -i 'node group'
```

Discovery and capacity resolution are logged at `V(4)`, which the `hydra-wl0`
deployment sets. At default verbosity a pool the autoscaler skipped and a pool
with nothing to do look identical — including a pool skipped for missing RBAC.

The annotations as applied:

```sh
kubectl get machinedeployment -o custom-columns=\
'NAME:.metadata.name,REPLICAS:.spec.replicas,MIN:.metadata.annotations.cluster\.x-k8s\.io/cluster-api-autoscaler-node-group-min-size,MAX:.metadata.annotations.cluster\.x-k8s\.io/cluster-api-autoscaler-node-group-max-size'
```

## What is verified, and what is not

**Verified.** The flag names, defaults and semantics above were read from the
Cluster Autoscaler v1.35.2 source, which is the version `hydra-wl0` runs — not
from documentation that might describe a different release. The deployed flag
set (`--scale-down-enabled=false`, no `--enforce-node-group-min-size`) was read
from the live manifest in `hydra-gitops`.

**Not verified.** No pool in these examples has been applied to a cluster, so
the autoscaler has never actually discovered one of them as a node group with
these bounds. Discovery was proven in PET-10 against the management cluster's own
pool, not against these. Applying the compute pool at `replicas: 0` is the cheap
way to close that — it creates no virtual machines until a pod pends — and the
scale-up that follows is PET-12.
