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
	ProviderOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "provider_operation_duration_seconds",
			Help:      "Time spent in infrastructure backend operations, by operation and outcome.",
			// Machine creation is seconds, not milliseconds: it clones a disk,
			// builds an ISO and defines a domain. The default buckets top out at
			// 10s and would put every create in +Inf.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80, 160},
		},
		[]string{"operation", "outcome"},
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
	// stage says where, reason says why, and terminal says whether retrying can
	// help -- the last being the one an operator most needs, since it is the
	// difference between waiting and intervening.
	MachineFailureTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "machine_failure_total",
			Help:      "Reconciles that ended in an error, by stage, reason and whether the error is terminal.",
		},
		[]string{"stage", "reason", "terminal"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ProviderOperationDuration,
		MachineWaitTotal,
		MachineFailureTotal,
	)
}
