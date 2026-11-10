# Moving clusters between management clusters

`clusterctl move` carries a namespace's Cluster API objects from one management
cluster to another: the `Cluster`, its machines and templates, the Hydra objects
behind them and the Secrets that go with them. The VMs stay where they are, and
the target's provider takes them over.

Until PET-76, none of that worked for Hydra. The move skipped every Hydra object
without a warning, left each cluster's hypervisor credentials behind, and the
machines it did carry could never find their VMs again. Read this page before a
move. Everything below is checked before anything changes.

## Before the move

**1. The source runs a provider with PET-76, and every machine has reconciled
under it.** A machine's VM is named from the object's UID, and a move gives
every object a new one. The provider records the UID the name came from in the
`hydramachine.infrastructure.cluster.x-k8s.io/identity-uid` annotation, and the
owner on the VM itself, with the owner it last confirmed in
`hydramachine.infrastructure.cluster.x-k8s.io/owner-uid`. The move carries both
annotations across. They are written the first time each machine reconciles
after the upgrade, and after each move, and a paused cluster's machines do not
reconcile. This must print nothing:

```bash
kubectl get hydramachines -A -o json | jq -r '.items[]
  | (.metadata.annotations // {}) as $a
  | select(($a["hydramachine.infrastructure.cluster.x-k8s.io/identity-uid"] // "") == ""
      or ($a["hydramachine.infrastructure.cluster.x-k8s.io/owner-uid"] // "") != .metadata.uid)
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

The identity is the one that cannot be missing. A machine moved without
`identity-uid` cannot be recovered by the provider: its VM's name hashes the UID
it was made under, and nothing else the move carries says what that was. An
operator who still has that UID can recover it; see
[If a machine was moved without its identity](#if-a-machine-was-moved-without-its-identity).
One moved without `owner-uid` still takes its VM over, from the owner the VM
records. The check asks for both so the next move, and the one after it, start
from a confirmed owner.

**2. The Hydra CRDs carry `clusterctl.cluster.x-k8s.io`.** Move looks up the
types to move by that label and nothing else. The release manifest sets it. A
provider installed from older manifests needs it added on both clusters:

```bash
for c in hydraclusters hydramachines hydramachinetemplates; do
  kubectl label crd "$c.infrastructure.cluster.x-k8s.io" clusterctl.cluster.x-k8s.io= --overwrite
done
```

**3. Every hypervisor credentials Secret carries `clusterctl.cluster.x-k8s.io/move`.**
Move takes a Secret with no owner along only when its name is
`<cluster>-<purpose>` for a purpose Cluster API defines, such as `kubeconfig`
or `ca`. A HydraCluster's `credentialsSecretRef` (see
[hypervisors.md](hypervisors.md)) is yours and is named however you like, so
label it. The provider does not label it for you, because it does not modify
your objects:

```bash
kubectl label secret <name> -n <namespace> clusterctl.cluster.x-k8s.io/move=
```

**4. The target runs the same provider version**, with its own
`libvirt-config` ConfigMap and, if the flags connection uses TLS, its own
`libvirt-pki` Secret in the provider's namespace. Those belong to the manager,
not to any cluster, and are not moved.

**5. You know what the namespace holds.** Move has no per-cluster filter: it
moves every cluster in the namespace together.

Then dry-run it, and read the plan. It must list a `HydraCluster` for each
`Cluster`, a `HydraMachine` for each `Machine`, and each credentials Secret:

```bash
clusterctl move --dry-run -n <namespace> --to-kubeconfig <target> -v 3
```

## After the move

Each moved HydraMachine takes its VM over the first time it reconciles on the
target. It logs `Took over a machine moved from another management cluster`,
and from then on only that object can manage or delete the VM. Nothing is
recreated. The providerID and the VM are the same as before.

## If a machine was moved without its identity

A machine moved before it recorded `identity-uid` (from a provider without
PET-76, or before it had reconciled under one) arrives with a new UID, so the VM
name it derives is not its VM's. It refuses to act, and touches nothing:

```
Ready=False ProvisioningFailed: terminal provider failure: providerID resolves to machine
"<namespace>-<name>-<old hash>", which is not owned by this HydraMachine (expected
"<namespace>-<name>-<new hash>") ...
```

It stays that way. Deleting it does not help: the provider refuses the delete
for the same reason and keeps its finalizer, so the VM is safe but the object
never goes away. The provider cannot fix this, but you can, if you still have
the UID the object had **before** the move. Places it can survive:

- **Prometheus**, if it scraped this provider's kube-state-metrics config
  (`config/metrics/`). Every `capi_hydramachine_*` series carries the object's
  `uid`. Query the time before the move:
  `capi_hydramachine_info{namespace="<namespace>",name="<name>"}`.
- A manifest dump or backup of the source objects, taken before the move.
- An etcd backup of the source management cluster.

Recover each machine in turn:

**1. Make sure no other copy exists.** The object it was moved from must be
gone, and no other management cluster may hold a copy of the namespace. The VM
records no owner yet, so the first copy to claim it wins.

**2. Check that the VM records no owner.** The record is written to the VM's
persistent definition only, so read the inactive XML; the live one never shows
it:

```bash
virsh dumpxml --inactive <vm name> | grep -o '<hydra:owner[^>]*>'
```

This must print nothing. If it names an owner, stop: the VM belongs to another
object, and this procedure is not for it.

**3. Prove the UID is the one the name was made from.** The VM name ends in the
first 20 hex digits of `sha256("<namespace>/<name>/<uid>")`. They must match
the old hash in the error:

```bash
printf '%s' '<namespace>/<name>/<old uid>' | sha256sum | cut -c1-20   # macOS: shasum -a 256
```

Do not try a UID that does not match. A wrong one is refused like the original,
but a match is the only evidence that this VM is the machine's.

**4. Record it** on the HydraMachine on the target:

```bash
kubectl annotate hydramachine <name> -n <namespace> \
  hydramachine.infrastructure.cluster.x-k8s.io/identity-uid=<old uid>
```

On its next reconcile, the provider finds the VM under that name and claims it
from nobody. It logs `Recorded owner on a machine that had none recorded`, sets
`owner-uid` to the object's own UID, and the machine goes `Ready`. The VM is not
restarted or recreated. From then on the machine is like any other: it can be
deleted, and it can be moved again.

If it is still refused, remove the annotation (`identity-uid-`) and recheck the
steps above. Never overwrite an `identity-uid` the provider recorded itself.

## What not to do

- **Do not undo a move by deleting the copies.** Move back instead. A moved
  object that has not yet taken its VM over refuses to delete it, so a failed
  or abandoned move cannot destroy a VM the source still runs. One that has
  taken over will delete it, because by then it is the owner.
- **Do not re-apply old copies of moved objects** (a backup, a manifest dump)
  to the cluster they left. They are refused as soon as they reconcile, because
  the VM records another owner, but they will not clean themselves up either.
- **Restoring a backup into another live management cluster is a move.** A
  backup of the current objects carries what a move carries, so its copies take
  the machines over, and the cluster the backup came from can then no longer
  manage them. Restore only where the original is gone or paused for good.
- **Do not put the identity annotation in a template's metadata.** Cluster API
  copies template annotations onto existing machines, which would rename every
  one of them.
- **Do not force-move a standalone HydraMachine before it has a providerID.**
  It arrives naming another object's identity with nothing to show it owns
  anything, and is refused.
