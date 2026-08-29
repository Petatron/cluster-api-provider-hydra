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

// Package fake provides an in-memory MachineProvider for testing the
// reconciler without a hypervisor.
//
// This exists so reconciler behaviour -- idempotency, finalizers, condition
// transitions, what happens when a machine vanishes underneath us -- can be
// tested exhaustively and deterministically. Several of those cases are
// difficult to provoke against real libvirt and trivial to provoke here.
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// Provider is an in-memory MachineProvider.
type Provider struct {
	mu       sync.Mutex
	machines map[string]*providers.MachineState // keyed by ID
	byName   map[string]string                  // name -> ID
	nextID   int

	// CreateCalls counts every Create, including idempotent repeats, so a test
	// can assert that a repeated reconcile did not create a second machine.
	CreateCalls int
	DeleteCalls int

	// Injected failures. Set to make the corresponding call fail.
	CreateErr error
	GetErr    error
	FindErr   error
	DeleteErr error

	// ReadyOnCreate controls whether machines report Ready immediately. Real
	// backends usually do not, so the default of false is the honest one.
	ReadyOnCreate bool

	// partials are names that have leftover resources but no machine -- a
	// volume created before DomainDefineXML failed. DeleteByName must remove
	// these; FindByName cannot see them.
	partials map[string]struct{}
}

// New returns an empty fake provider.
func New() *Provider {
	return &Provider{
		machines: map[string]*providers.MachineState{},
		byName:   map[string]string{},
		partials: map[string]struct{}{},
	}
}

var _ providers.MachineProvider = (*Provider)(nil)

// Name implements providers.MachineProvider.
func (p *Provider) Name() string { return "fake" }

// Create implements providers.MachineProvider, including its idempotency
// requirement on spec.Name.
func (p *Provider) Create(_ context.Context, spec providers.MachineSpec) (*providers.MachineState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.CreateCalls++
	if p.CreateErr != nil {
		return nil, p.CreateErr
	}

	if id, ok := p.byName[spec.Name]; ok {
		return copyState(p.machines[id]), nil
	}

	p.nextID++
	id := fmt.Sprintf("fake-%d", p.nextID)
	state := &providers.MachineState{
		ID:    id,
		Ready: p.ReadyOnCreate,
		Addresses: []providers.Address{
			{Type: providers.AddressTypeHostname, Address: spec.Name},
		},
	}
	p.machines[id] = state
	p.byName[spec.Name] = id
	return copyState(state), nil
}

// Get implements providers.MachineProvider.
func (p *Provider) Get(_ context.Context, id string) (*providers.MachineState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.GetErr != nil {
		return nil, p.GetErr
	}
	state, ok := p.machines[id]
	if !ok {
		return nil, providers.ErrNotFound
	}
	return copyState(state), nil
}

// FindByName implements providers.MachineProvider.
func (p *Provider) FindByName(_ context.Context, name string) (*providers.MachineState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.FindErr != nil {
		return nil, p.FindErr
	}
	id, ok := p.byName[name]
	if !ok {
		return nil, providers.ErrNotFound
	}
	return copyState(p.machines[id]), nil
}

// Delete implements providers.MachineProvider. Deleting an absent machine
// succeeds, as the interface requires.
func (p *Provider) Delete(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.DeleteCalls++
	if p.DeleteErr != nil {
		return p.DeleteErr
	}
	p.deleteLocked(id)
	return nil
}

// DeleteByName implements providers.MachineProvider.
func (p *Provider) DeleteByName(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.DeleteCalls++
	if p.DeleteErr != nil {
		return p.DeleteErr
	}
	delete(p.partials, name)
	if id, ok := p.byName[name]; ok {
		p.deleteLocked(id)
	}
	return nil
}

func (p *Provider) deleteLocked(id string) {
	state, ok := p.machines[id]
	if !ok {
		return
	}
	for name, mappedID := range p.byName {
		if mappedID == state.ID {
			delete(p.byName, name)
			break
		}
	}
	delete(p.machines, id)
}

// AddPartial records leftover resources for a name that has no machine, so a
// test can assert DeleteByName still cleans them up.
func (p *Provider) AddPartial(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partials[name] = struct{}{}
}

// PartialCount returns how many name-keyed leftovers currently exist.
func (p *Provider) PartialCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.partials)
}

// SetReady marks a machine ready, simulating a backend finishing provisioning
// between one reconcile and the next.
func (p *Provider) SetReady(id string, ready bool, addrs ...providers.Address) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if state, ok := p.machines[id]; ok {
		state.Ready = ready
		if len(addrs) > 0 {
			state.Addresses = addrs
		}
	}
}

// Count returns how many machines currently exist.
func (p *Provider) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.machines)
}

// copyState defends against a caller mutating the fake's internal state, which
// would make tests pass for the wrong reason.
func copyState(s *providers.MachineState) *providers.MachineState {
	if s == nil {
		return nil
	}
	out := *s
	out.Addresses = append([]providers.Address(nil), s.Addresses...)
	return &out
}
