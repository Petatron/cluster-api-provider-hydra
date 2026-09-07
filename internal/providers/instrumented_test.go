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
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/Petatron/cluster-api-provider-hydra/internal/metrics"
)

// The outcome label is the whole reason this decorator earns its place. A graph
// that cannot separate "the hypervisor refused" from "the hypervisor was not
// reachable" cannot tell a misconfigured cluster from a flaky one, which is the
// question an operator is actually asking.
func TestClassifyOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, outcomeSuccess},
		{"terminal", fmt.Errorf("%w: pool missing", ErrTerminal), outcomeTerminal},
		// Not a failure: Create's idempotency check and deletion both ask for
		// machines that may legitimately not exist. Counting those as errors
		// would make a healthy provider look broken.
		{"not found", fmt.Errorf("%w: no such machine", ErrNotFound), outcomeNotFound},
		{"unreachable", errors.New("dial unix: connection refused"), outcomeError},
		// Terminal wins when both are present: it is the actionable half.
		{"terminal wrapping not-found", fmt.Errorf("%w: %w", ErrTerminal, ErrNotFound), outcomeTerminal},
	} {
		if got := ClassifyOutcome(tc.err); got != tc.want {
			t.Errorf("ClassifyOutcome(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A decorator that quietly changed behaviour would be worse than no metrics at
// all, so the pass-through is asserted rather than assumed.
func TestInstrumentedPassesResultsThrough(t *testing.T) {
	backend := &recordingProvider{state: &MachineState{ID: "abc"}}
	p := NewInstrumented(backend)

	state, err := p.Create(t.Context(), MachineSpec{Name: "worker-1"})
	if err != nil || state == nil || state.ID != "abc" {
		t.Fatalf("Create() = %v, %v; want the backend's own result", state, err)
	}
	if backend.lastSpec.Name != "worker-1" {
		t.Errorf("spec reached the backend as %q, want worker-1", backend.lastSpec.Name)
	}

	wantErr := errors.New("boom")
	backend.err = wantErr
	if _, err := p.Get(t.Context(), "abc"); !errors.Is(err, wantErr) {
		t.Errorf("Get() = %v, want the backend's error unchanged", err)
	}
}

// The dial is the operation most likely to hang -- an unreachable hypervisor
// consumes the whole timeout -- and it happens inside the constructor, before
// there is a provider to wrap. Without this it would be the only operation
// producing no sample at all, which is exactly the case the metric exists for.
func TestObserveDialRecordsASample(t *testing.T) {
	before := testutil.CollectAndCount(metrics.ProviderOperationDuration)

	ObserveDial(time.Now(), errors.New("dial unix: connection refused"))

	if after := testutil.CollectAndCount(metrics.ProviderOperationDuration); after <= before {
		t.Fatalf("series count %d -> %d; the dial recorded nothing", before, after)
	}
	// Labelled as a failed reach, not as a refusal: an unreachable hypervisor is
	// the thing an operator waits out, not the thing they go and fix.
	obs, err := metrics.ProviderOperationDuration.GetMetricWithLabelValues("Dial", outcomeError)
	if err != nil {
		t.Fatalf("no Dial/%s series: %v", outcomeError, err)
	}
	var m dto.Metric
	if err := obs.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("reading the histogram: %v", err)
	}
	if got := m.GetHistogram().GetSampleCount(); got != 1 {
		t.Errorf("Dial/%s sample count = %d, want 1", outcomeError, got)
	}
}

func TestNewInstrumentedTolerantOfNil(t *testing.T) {
	// The factory in main can fail before a provider exists; wrapping nil must
	// not turn that into a nil-pointer dereference two layers away.
	if got := NewInstrumented(nil); got != nil {
		t.Errorf("NewInstrumented(nil) = %v, want nil", got)
	}
}

type recordingProvider struct {
	MachineProvider
	state    *MachineState
	err      error
	lastSpec MachineSpec
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Create(_ context.Context, spec MachineSpec) (*MachineState, error) {
	p.lastSpec = spec
	return p.state, p.err
}

func (p *recordingProvider) Get(_ context.Context, _ string) (*MachineState, error) {
	return p.state, p.err
}
