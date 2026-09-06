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

package providers

import (
	"context"
	"errors"
	"time"

	"github.com/Petatron/cluster-api-provider-hydra/internal/metrics"
)

// Instrumented wraps a MachineProvider so every backend call is timed.
//
// A decorator rather than instrumentation inside the backend, for two reasons.
// It measures what the controller actually waits on, including the connection
// handling and timeouts the backend wraps around each RPC -- which is the
// number that matters when a reconcile is slow. And it puts the metric names in
// one place instead of once per backend, so a second backend cannot quietly
// publish a different set.
//
// The operation label is the interface method name, so its cardinality is fixed
// by the interface and cannot grow at runtime.
type Instrumented struct {
	MachineProvider
}

// NewInstrumented returns p wrapped in timing, or p unchanged if it is nil.
func NewInstrumented(p MachineProvider) MachineProvider {
	if p == nil {
		return nil
	}
	return &Instrumented{MachineProvider: p}
}

// The outcome label's values, named so the decorator and its tests cannot
// disagree about the spelling of a label a dashboard will depend on.
const (
	outcomeSuccess  = "success"
	outcomeTerminal = "terminal"
	outcomeNotFound = "not_found"
	outcomeError    = "error"
)

// ClassifyOutcome describes an error in the vocabulary the provider's metrics
// use.
//
// Exported because two different metrics need it -- backend operations here and
// machine failures in the controller -- and two independent classifications
// would eventually disagree about what "terminal" means, leaving a dashboard
// that reads one label against the other.
//
// The split is the point: "terminal" is a backend that refused and will refuse
// again, "error" is a backend that could not be reached and may work in a
// moment. Collapsing them would produce a graph that cannot distinguish a
// misconfigured cluster from a flaky hypervisor.
func ClassifyOutcome(err error) string {
	switch {
	case err == nil:
		return outcomeSuccess
	case errors.Is(err, ErrTerminal):
		return outcomeTerminal
	case errors.Is(err, ErrNotFound):
		// Not a failure. Create's idempotency check and deletion both ask for
		// machines that may legitimately not exist, and counting those as errors
		// would make a healthy provider look broken.
		return outcomeNotFound
	default:
		return outcomeError
	}
}

// ObserveDial records establishing a connection to the backend.
//
// It exists because that happens before a provider exists to be wrapped: the
// dial and handshake are inside the constructor, so without this the one
// operation most likely to hang -- an unreachable hypervisor, consuming the
// whole dial timeout -- would be the only one producing no sample at all, and
// the first slow reconcile could not be attributed to it.
//
// Narrowly named on purpose. A general exported observe() would become the
// escape hatch through which unbounded labels arrive.
func ObserveDial(start time.Time, err error) {
	observe("Dial", start, err)
}

func observe(operation string, start time.Time, err error) {
	metrics.ProviderOperationDuration.
		WithLabelValues(operation, ClassifyOutcome(err)).
		Observe(time.Since(start).Seconds())
}

func (p *Instrumented) Create(ctx context.Context, spec MachineSpec) (*MachineState, error) {
	start := time.Now()
	state, err := p.MachineProvider.Create(ctx, spec)
	observe("Create", start, err)
	return state, err
}

func (p *Instrumented) Delete(ctx context.Context, id string) error {
	start := time.Now()
	err := p.MachineProvider.Delete(ctx, id)
	observe("Delete", start, err)
	return err
}

func (p *Instrumented) Get(ctx context.Context, id string) (*MachineState, error) {
	start := time.Now()
	state, err := p.MachineProvider.Get(ctx, id)
	observe("Get", start, err)
	return state, err
}

func (p *Instrumented) FindByName(ctx context.Context, name string) (*MachineState, error) {
	start := time.Now()
	state, err := p.MachineProvider.FindByName(ctx, name)
	observe("FindByName", start, err)
	return state, err
}

func (p *Instrumented) DeleteByName(ctx context.Context, name string) error {
	start := time.Now()
	err := p.MachineProvider.DeleteByName(ctx, name)
	observe("DeleteByName", start, err)
	return err
}

func (p *Instrumented) EnsureInfrastructure(ctx context.Context, spec InfrastructureSpec) error {
	start := time.Now()
	err := p.MachineProvider.EnsureInfrastructure(ctx, spec)
	observe("EnsureInfrastructure", start, err)
	return err
}
