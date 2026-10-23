# Alerts

`config/prometheus/alerts.yaml` is a `PrometheusRule` for clusters built with
this provider. Each alert's `runbook_url` points at its section below.

## What the rules read

Everything is evaluated on the **management cluster**, from two sources:

- **This provider's metrics** (see [metrics.md](metrics.md)), scraped through
  the ServiceMonitor in `config/prometheus/monitor.yaml`. The scraper's
  ServiceAccount needs the `metrics-reader` ClusterRole.
- **kube-state-metrics with custom-resource-state configs** for Cluster API's
  objects and this provider's. Cluster API publishes its config in
  `core/config/metrics/crd-metrics-config.yaml` (`capi_cluster_*`,
  `capi_machine_*`, `capi_kubeadmcontrolplane_*`, …). This provider's is
  `config/metrics/crd-metrics-config.yaml` (`capi_hydracluster_*`,
  `capi_hydramachine_*`), in the same shape. kube-state-metrics takes one config
  file, so append this provider's `resources` to Cluster API's.
  `config/metrics/crd-clusterrole.yaml` lets kube-state-metrics read the Hydra
  objects.

**Label the rule for your Prometheus.** The `PrometheusRule` carries no
`release` label, and a Prometheus Operator whose `ruleSelector` requires one
ignores it silently: no error and no alerts. Add whatever your Prometheus
selects on, for example with a kustomize `labels:` entry, and confirm the
rules appear under Prometheus' *Alerts* page after applying. The same goes for
the ServiceMonitor and kube-state-metrics' monitor.

Rules over Cluster API objects only consider clusters whose infrastructure is
a `HydraCluster` and machines whose infrastructure is a `HydraMachine`, so the
file is safe on a management cluster shared with other providers.

Every alert is named `Hydra*`, and `severity` is `critical` (something is down
or about to be) or `warning` (someone has to act, but nothing is down yet).

## Objects still being built

A new control plane has no quorum and its machines are not Ready yet. The
etcd and control-plane alerts therefore ignore a KubeadmControlPlane in its
first hour, and the machine-readiness alerts ignore a HydraMachine in its first
30 minutes. One still not healthy after that is reported.

## What the rules cannot see

- **The management cluster itself.** If it, its Prometheus or its Alertmanager
  stops, these rules stop with it. Pair them with a dead-man's switch: route the
  always-firing `Watchdog` alert to an external heartbeat service that alerts
  when the heartbeat stops.
- **A network between sites.** When the management cluster loses the network
  to a workload cluster, several of these fire at once. Probing that path, and
  inhibiting the rest while it is down, is deployment-specific.
- **A VM stopped behind the provider's back, for up to 5 minutes.** The
  provider re-checks each healthy machine every 5 minutes and starts a domain
  it finds stopped. A stopped VM is usually back before any `for:` here
  expires. A VM that will not stay up shows as
  [HydraMachineNotReady](#hydramachinenotready) or
  [HydraNodeUnhealthy](#hydranodeunhealthy).

## Testing the rules

```bash
make test-alerts
```

This downloads a pinned `promtool` (checked against the Prometheus release's
checksums), lints the rules, and runs the unit tests in
`config/prometheus/alerts_test.yaml`. Every alert needs at least one case there
that expects it to fire, and a summary, description and section here.

## The alerts

### HydraProviderDown

**critical, 5m.** Prometheus has had no successful scrape of the provider's
metrics endpoint for 5 minutes, or has never had one.

While the manager is down nothing creates, repairs or deletes Hydra machines;
existing VMs keep running. Check the Deployment in
`cluster-api-provider-hydra-system`, then its log. If the pod is running, the
problem is the scrape: the ServiceMonitor's selector, or the scraper's
permission to read `/metrics`.

### HydraBackendUnreachable

**critical, 15m window + 5m.** Every call the provider made to one hypervisor in
the last 15 minutes failed to connect, and none got through. The `hypervisor`
label names it; with several, each is judged on its own, so one healthy host
does not hide another that is down. Past 16 hypervisors the rest share the label
`other` (see [metrics.md](metrics.md)) and are judged together, so among those a
healthy one does hide a dead one.

The provider calls the backend at least every 5 minutes per machine, so this is
not a quiet period. The hypervisor is down, the network to it is, or its TLS
credentials are being refused. Running VMs may be unaffected. Look for
`libvirt backend` in the manager's log to see the address it uses, then check
that host directly.

A burst of errors while some calls still succeed does not fire this. That is
[HydraProviderReconcileErrors](#hydraproviderreconcileerrors).

### HydraProviderReconcileErrors

**warning, 30m.** A controller has returned reconcile errors in every
15-minute window for 30 minutes.

An occasional error is normal: an update conflict is retried at once. A
sustained one means an object cannot make progress. The manager's log names the
object and the error.

### HydraMachineTerminalFailure

**warning.** The backend refused an operation in a way that will not change on
retry (`hydra_machine_failure_total{cause="terminal"}`), for example a disk
size the storage pool cannot hold.

The counter's series is created by its first failure, already at 1, so the
rule also fires on a series that did not exist 15 minutes earlier: the first
failure counts, not only the second.

Someone has to change something. The affected HydraMachine's `ProvisioningFailed`
condition carries the message; see
[HydraMachineProvisioningFailed](#hydramachineprovisioningfailed).

### HydraClusterInfrastructureFailed

**warning, 5m.** A HydraCluster's `InfrastructureFailed` condition is `True`: a
cluster-scoped prerequisite is missing and will not appear by itself, such as a
storage pool that does not exist or a base image missing from it.

No new machine can be created in that cluster until it is fixed. Existing
machines are unaffected. The condition's message names what is missing.

### HydraClusterNotReady

**warning, 15m.** A HydraCluster's `Ready` condition has not been `True` for 15
minutes, so the cluster cannot take new machines.

If [HydraClusterInfrastructureFailed](#hydraclusterinfrastructurefailed) is
firing too, start there. Otherwise the `Ready` condition's message says what is
being waited for.

### HydraMachineNotReady

`HydraControlPlaneMachineNotReady` (**critical**) and `HydraMachineNotReady`
(**warning**), **10m.** A HydraMachine's `Ready` condition has not been `True`
for 10 minutes: its VM is missing, stopped, or the provider cannot confirm it
is running.

The condition's message says which. A control-plane machine is critical because
each one lost takes etcd closer to losing quorum.

The provider starts a stopped domain at its next check. If this still fires,
the VM is failing to stay up, or the provider cannot reach the backend; check
[HydraBackendUnreachable](#hydrabackendunreachable).

### HydraMachineProvisioningFailed

**warning, 1m.** A HydraMachine's `ProvisioningFailed` condition is `True`: the
provider has given up on it.

The message names the cause. Fix it, then delete the Machine (not the
HydraMachine); Cluster API creates a replacement.

### HydraClusterAPIUnreachable

**critical, 3m.** Cluster API's connection probe from the management cluster
to a Hydra cluster's API endpoint has been failing for 3 minutes.

Either the cluster's control plane, or the address in front of it, is down, or
the network between the two clusters is. Until it recovers, Cluster API cannot
see the cluster's nodes, so node and control-plane alerts for that cluster are
unreliable. From the management cluster, try the endpoint in the Cluster's
`spec.controlPlaneEndpoint`.

### HydraClusterNotAvailable

**warning, 15m.** Cluster API's summary condition for a Hydra cluster has not
been `True` for 15 minutes.

More specific alerts usually fire first; this one catches whatever they miss.
The Cluster's `Available` condition message lists each reason.

A Cluster whose control plane Cluster API does not manage (no `controlPlaneRef`,
for example an adopted cluster) can never become Available, so the rule leaves
it out. Watch such a cluster's nodes some other way.

### HydraNodeUnhealthy

`HydraControlPlaneNodeUnhealthy` (**critical**) and `HydraNodeUnhealthy`
(**warning**), **5m.** A Machine's `NodeHealthy` condition has not been `True`
for 5 minutes. The node is NotReady, unreachable, or (for a control plane) its
static pods are failing.

Only machines that already have a node and are not being deleted count, so a
machine still booting or a scale-down does not fire this. Neither do machines of
a cluster whose control plane Cluster API has not initialized (an adopted
cluster with no `controlPlaneRef` never is): Cluster API cannot inspect their
nodes, so `NodeHealthy` stays Unknown and says nothing about them. `node_name` names the
node. A MachineHealthCheck, if there is one, may already be replacing it.

If [HydraClusterAPIUnreachable](#hydraclusterapiunreachable) fires for the same
cluster, start there: Cluster API cannot see any node then.

### HydraNodeFlapping

**warning, 30m window.** A Hydra machine's `NodeHealthy` condition changed at
least four times in 30 minutes, i.e. at least two separate unhealthy episodes,
each too short for [HydraNodeUnhealthy](#hydranodeunhealthy).

The usual cause is a VM that keeps dying: the provider starts a stopped domain
at its next check, every 5 minutes, so the node is back before the 5-minute
alert fires. A kubelet that keeps failing looks the same. Look at the VM's
console log on the hypervisor and the node's kubelet log.

### HydraEtcdUnhealthy

**critical, 5m.** A Hydra cluster's KubeadmControlPlane reports
`EtcdClusterHealthy` not `True` for 5 minutes: a member is down, unreachable,
or in alarm.

Another failure may cost quorum. Before changing anything, confirm the latest
etcd backup is recent. The KubeadmControlPlane's condition message, and each
control-plane Machine's `EtcdMemberHealthy`, say which member.

### HydraControlPlaneUnavailable

**critical, 2m.** Cluster API reports a Hydra cluster's KubeadmControlPlane not
`Available`: no API server is healthy, or, with stacked etcd, etcd has lost
quorum. The cluster's API is down or refuses writes. The condition's message
says which.

This reads Cluster API's own judgement rather than counting Ready machines
against the desired number. That count cannot tell a lost quorum from a
scale-up still in progress: a single control plane being grown to three has
one of three Ready for minutes, and nothing is wrong.

Do not delete control-plane machines to "fix" this; removing members from a
cluster without quorum makes recovery harder. Bring the missing machines back
if they can come back, otherwise restore from an etcd backup.

### HydraControlPlaneDegraded

**warning, 15m.** Fewer control-plane machines are Ready than desired while
the control plane is still Available. One more failure may take it away.

A scale-up, rollout or remediation normally restores the count well within 15
minutes.
If not, look at the KubeadmControlPlane's `MachinesReady` condition.
