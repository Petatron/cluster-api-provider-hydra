/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package metrics holds the provider's own Prometheus instrumentation.
//
// Deliberately small. controller-runtime already publishes reconcile counts,
// durations and error totals per controller, so anything this package adds has
// to answer a question those cannot -- which in practice means: when a machine
// is slow or stuck, WHICH stage is responsible.
//
// Every label here has a bounded, enumerable set of values. No machine name, no
// namespace, no providerID: a metric that grows a new series per machine is a
// memory leak that arrives months later, and this provider exists to create
// machines by the hundred.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "hydra"

var (
	// ProviderOperationDuration times calls into the infrastructure backend.
	//
	// This is the difference between "the reconcile took 40 seconds" and "the
	// reconcile took 40 seconds because defining a domain took 39 of them".
	// controller-runtime can tell you the first; nothing but this can tell you
	// the second.
	//
	// outcome separates a backend that refused from a backend that could not be
	// reached, because the two call for completely different responses -- the
	// same distinction ErrTerminal draws in the code.
	//
	// hypervisor names the backend endpoint the call went to (PET-38), so an
	// unreachable host is identifiable once a manager talks to more than one.
	// The connection pool hands it out and keeps it bounded: at most 16
	// hypervisors are labelled by name at once, and any more share "other", so
	// clusters naming hypervisors cannot grow it without limit. A label's
	// series are dropped once no cluster uses that hypervisor. It is a label,
	// not an identity: the same host reached with two sets of credentials, or
	// by IP and by name, shares or splits values.
	ProviderOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "provider_operation_duration_seconds",
			Help:      "Time spent in infrastructure backend operations, by operation, outcome and hypervisor.",
			// Machine creation is seconds, not milliseconds: it clones a disk,
			// builds an ISO and defines a domain. The default buckets top out at
			// 10s and would put every create in +Inf.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80, 160},
		},
		[]string{"operation", "outcome", "hypervisor"},
	)

	// MachineWaitTotal counts reconciles that ended in a wait rather than an
	// error or a success.
	//
	// A counter rather than a gauge of machines currently waiting, because a
	// gauge would need state this controller does not keep and would go stale
	// the moment a reconcile was missed. The rate of this, split by reason, is
	// what says whether a stalled fleet is waiting on bootstrap data, on the
	// cluster, or on an owner that is never coming.
	MachineWaitTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "machine_wait_total",
			Help:      "Reconciles that ended waiting for something, by reason.",
		},
		[]string{"reason"},
	)

	// MachineFailureTotal counts reconciles that ended in an error.
	//
	// stage says where it happened; cause says what kind of failure it was, in
	// the same vocabulary ProviderOperationDuration uses, so the two can be read
	// against each other.
	//
	// An earlier version carried reason and terminal alongside stage. That was
	// three labels holding two facts: reason was always "<stage>Failed" or
	// "<stage>FailedRetrying", so it was fully determined by the other two and
	// explained nothing on its own. cause is the dimension that actually varies
	// independently -- whether the backend refused, could not be reached, or
	// reported the thing missing -- and terminality is recoverable from it.
	MachineFailureTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "machine_failure_total",
			Help:      "Reconciles that ended in an error, by stage and cause.",
		},
		[]string{"stage", "cause"},
	)
)

// ProviderConnections is the number of backend connections the manager holds
// open (PET-38). Connections open lazily, on the first reconcile that needs
// one, so this reads 0 until then. Once clusters can name hypervisors of their
// own it is one per target in use -- a hypervisor and the credentials used to
// reach it, so one host reached with two sets counts twice -- plus the
// flag-configured default, and a cluster deleted or moved brings it down when
// nobody else uses that connection, which is how releasing one is observable.
var ProviderConnections = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: namespace,
	Name:      "provider_connections",
	Help:      "Backend connections the manager holds open.",
})

func init() {
	metrics.Registry.MustRegister(
		ProviderConnections,
		ProviderOperationDuration,
		MachineWaitTotal,
		MachineFailureTotal,
	)
}
