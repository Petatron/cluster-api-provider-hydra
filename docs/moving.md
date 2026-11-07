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
  | select((.metadata.annotations["hydramachine.infrastructure.cluster.x-k8s.io/owner-uid"] // "") != .metadata.uid)
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

A machine moved without it cannot be recovered by the provider, because
nothing the move carries says which UID its VM was named for.

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

## What not to do

- **Do not undo a move by deleting the copies.** Move back instead. A moved
  object that has not yet taken its VM over refuses to delete it, so a failed
  or abandoned move cannot destroy a VM the source still runs. One that has
  taken over will delete it, because by then it is the owner.
- **Do not re-apply old copies of moved objects** (a backup, a manifest dump)
  to the cluster they left. They are refused as soon as they reconcile, because
  the VM records another owner, but they will not clean themselves up either.
- **Do not put the identity annotation in a template's metadata.** Cluster API
  copies template annotations onto existing machines, which would rename every
  one of them.
- **Do not force-move a standalone HydraMachine before it has a providerID.**
  It arrives naming another object's identity with nothing to show it owns
  anything, and is refused.
