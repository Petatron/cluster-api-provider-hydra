# Hypervisors

A HydraCluster can name the hypervisor its machines are created on (PET-38).
One that names none uses the hypervisor the manager's `--libvirt-*` flags
configure, exactly as before. So one manager can run clusters on several
hypervisors.

## Naming one

```yaml
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: HydraCluster
spec:
  hypervisor:
    address: hv2.lab.example:16514   # host or host:port; [IPv6]:port; default port 16514
    credentialsSecretRef:
      name: hv2-libvirt-pki          # in the HydraCluster's namespace
```

The connection is always TLS to `qemu:///system`. Plaintext is only available
manager-wide, through `--libvirt-insecure`, for a trusted tunnel. A per-cluster
switch would make it too easy to send privileged RPCs in the clear. The
hypervisor's certificate must be valid for the host in `address`.

**Who may name a hypervisor.** Anyone who can create or edit a HydraCluster can
make the manager dial a host and port of their choosing, and the dial's error
appears in the cluster's status. That is fine while HydraClusters are written
by the operators who run the hypervisors, which is the model today. If that
ever changes, restrict who may write the field (an admission policy, say)
before handing out HydraCluster write access.

The Secret holds the same three files libvirt reads from its PKI directory:

```bash
kubectl create secret generic hv2-libvirt-pki \
  --from-file=cacert.pem --from-file=clientcert.pem --from-file=clientkey.pem
```

It is read uncached on every reconcile that connects, so the manager holds no
Secret informer. A Secret that is missing, lacks a key, or holds a certificate
and key that don't match is reported terminally on the cluster, as
`InfrastructureFailed` naming the Secret. Its machines only wait; see below.

**Rotating credentials:** replace the Secret's contents, or point
`credentialsSecretRef` at a new Secret. The next reconcile opens a connection
with the new credentials, and the old connection closes once nothing uses it.

The manager's `--libvirt-storage-pool` and `--libvirt-base-image` defaults still
apply to a cluster that names neither. They are names, so they must exist on
that cluster's hypervisor; the cluster's infrastructure check says so if they
don't.

## Where a machine lives

**Each machine records the hypervisor it lives on.** The annotation
`hydramachine.infrastructure.cluster.x-k8s.io/hypervisor` is written before
Create, in the same patch as the storage-pool annotation. A machine without it
that may have been made lives on the flag-configured hypervisor. "May have been
made" means it has a providerID, or it records a storage pool, which is written
just before a Create that might have left a domain or volumes behind. Only a
machine nothing was ever attempted for takes its cluster's hypervisor. Every
operation on a machine, teardown included, goes to the recorded hypervisor,
never to whatever its cluster names by then. Naming a hypervisor never moves a
machine.

So a storage-pool annotation set by hand, or copied from a
HydraMachineTemplate's metadata, puts a new machine on the flag connection.
Leave both annotations to the controller.

The credentials come from the machine's HydraCluster, which must name that same
hypervisor. The HydraCluster is found through the owner Machine or, if that is
gone, through the machine's own cluster-name label. If it is gone, unreadable,
or names another hypervisor, the machine waits with
`WaitingForHypervisorCredentials`. It never falls back to the flag connection,
which may be another hypervisor where a domain of the same name belongs to
someone else. Cluster API deletes a HydraCluster only after its machines, so
normal teardown never hits this. If you delete a HydraCluster by hand first,
recreate it to let its machines finish.

A credentials Secret that is missing or broken also makes the cluster's
machines wait, with the same reason. The HydraCluster is the object that
reports it as `InfrastructureFailed`. Running machines are not marked
`ProvisioningFailed` over a Secret, which might be in the middle of being
replaced.

## Adding a hypervisor to an existing cluster

`spec.hypervisor` may be added to a cluster that already has machines. Before
anything uses it, the HydraCluster controller checks that every one of the
cluster's machines is already there: the domain must exist with the machine's
name and with the ID its providerID records. Only then does it confirm the
hypervisor in `status.hypervisor` and record it on each machine. Until it is
confirmed, new machines wait with `WaitingForHypervisor`.

Machines not made yet, and machines being deleted, are not checked. A machine
whose Create on the flag connection has begun but has no providerID yet makes
confirmation wait until it has one. The cluster stays Ready meanwhile, with the
wait in its `Ready` message, because that machine needs a Ready cluster to
finish being created. If its Create keeps failing there (a full or missing
pool, say), it finishes nowhere: delete it, and its replacement is created on
the named hypervisor once that is confirmed. A machine found anywhere else, or one
recording another hypervisor, refuses confirmation: the cluster reports
`InfrastructureFailed` naming the machines, and nothing is recorded. Machines
never move between hypervisors, so name the one they are on.

One race is left. A machine whose reconcile read the cluster just before you
added `spec.hypervisor` can begin a Create on the flag connection just after
confirmation. If that is a different hypervisor, the cluster then reports
`InfrastructureFailed` naming it, and blocks new machines. Delete that machine:
its replacement is created on the confirmed hypervisor. When the flags already
reach the named hypervisor, which is the usual case below, the machine is found
there and recorded like the rest.

**Until it is confirmed, `spec.hypervisor` can be corrected or removed.** After
that it is fixed: it can't be removed and its address can't change, because the
cluster's machines are recorded against it. Only the credentials may change.

Confirmation also waits for the cluster's infrastructure check (its storage
pool, and base image if it names one) to pass on the named hypervisor. That is
the only check a cluster with no machines gets, so an address that answers but
is not the hypervisor you meant stays correctable as long as it lacks the pool.
If it does have the pool, confirmation happens on the first reconcile, so
check `status.hypervisor` right after naming one.

The typical use is the hypervisor the flags already reach, now with credentials
of its own. That way the cluster stops depending on the manager's flags without
moving anything.

## Connections

The manager holds one connection per hypervisor and set of credentials. A
cluster and each of its machines own the connection they use, and it closes
when the last of them lets go: so a deleted cluster's connection stays open
until its last machine has been torn down. `hydra_provider_connections` counts
open connections, and the `hypervisor` label on
`hydra_provider_operation_duration_seconds` names each one (see
[metrics.md](metrics.md)).
