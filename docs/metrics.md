# Metrics

The manager serves Prometheus metrics on `:8443` over HTTPS, protected by
`TokenReview`/`SubjectAccessReview` — a scraper needs a token bound to the
`metrics-reader` ClusterRole the deployment ships. `config/prometheus` contains
a ServiceMonitor for a Prometheus Operator install; nothing here depends on one
being present.

Everything below is in addition to what controller-runtime already publishes.
Read those first: `controller_runtime_reconcile_total`,
`controller_runtime_reconcile_time_seconds` and
`controller_runtime_reconcile_errors_total`, labelled by controller, answer
"how often, how long, how many errors" without any help from this provider.

## What this provider adds

### `hydra_provider_operation_duration_seconds`

Histogram. Labels: `operation`, `outcome`.

Time spent inside the infrastructure backend. This is the difference between
*"the reconcile took 40 seconds"* and *"defining the domain took 39 of them"* —
controller-runtime can tell you the first, only this can tell you the second.

`operation` is an interface method name, so its values are fixed by the code:
`Dial`, `Create`, `Get`, `FindByName`, `Delete`, `DeleteByName`,
`EnsureInfrastructure`. `Dial` is connection establishment, which happens inside
the backend constructor rather than through the interface — it is included
because an unreachable hypervisor consumes the whole dial timeout, and it would
otherwise be the one operation producing no sample.

`outcome` carries a distinction worth understanding before building an alert on
it:

| value | meaning | what an operator does |
| --- | --- | --- |
| `success` | | nothing |
| `terminal` | the backend refused and will refuse again | intervene: this will not clear |
| `error` | the backend could not be reached | wait: this may clear on its own |
| `not_found` | the thing asked about does not exist | usually nothing — see below |

`not_found` is **not** a failure. `Create` asks whether a machine already exists
as its idempotency check, and deletion asks for machines that may legitimately
be gone. Alerting on it would fire constantly against a healthy provider.

Buckets run to 160s. Machine creation clones a disk, builds an ISO and defines a
domain; the client library's defaults top out at 10s and would put every create
in `+Inf`.

### `hydra_machine_wait_total`

Counter. Label: `reason`.

Reconciles that ended waiting for something rather than failing. The values are
the provider's four wait states: `WaitingForOwnerMachine`, `WaitingForCluster`,
`WaitingForClusterInfrastructure`, `WaitingForBootstrapData`.

A counter rather than a gauge of machines currently waiting, because a gauge
would need state the controller does not keep and would go stale the moment a
reconcile was missed. The **rate**, split by reason, is what says whether a
stalled fleet is blocked on bootstrap data, on its cluster, or on an owner that
is never coming.

None of these is an error. A machine waiting on CABPK to publish a Secret is
proceeding exactly as designed, and an alert that treats waiting as failure will
fire on every machine ever created.

### `hydra_machine_failure_total`

Counter. Labels: `stage`, `cause`.

Reconciles that ended in an error. `stage` is the phase — `Provisioning` or
`Deleting` — and `cause` uses the same vocabulary as `outcome` above, from the
same classifier, so the two metrics can be read against each other.

`cause="terminal"` is the one worth alerting on: it means a machine will not
recover without someone changing something.

## Series only appear once observed

A labelled Prometheus metric publishes nothing until a label combination has
been seen at least once, so a freshly started manager exposes only `Dial` and no
`hydra_machine_*` at all. That is correct behaviour, not a missing metric — an
absent `hydra_machine_failure_total` means nothing has failed.

Do not write `absent()` alerts against these.

## What is deliberately not here

**No machine name, namespace or providerID appears as a label anywhere.** Every
label value above is drawn from a set fixed by the code. A metric that grows a
new series per machine is a memory leak that arrives months later, in a provider
whose entire purpose is creating machines in quantity — and the identifiers are
already in the logs, correlated by object, where cardinality costs nothing.

**Boot-to-NodeReady latency is not measured.** It spans the workload cluster:
the provider knows when it created a machine, but node registration is Cluster
API setting `nodeRef` after the kubelet joins. Measuring it properly means
watching the owning Machine rather than the infrastructure object. Tracked
separately rather than approximated here.
