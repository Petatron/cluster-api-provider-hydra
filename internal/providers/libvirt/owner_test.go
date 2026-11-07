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

package libvirt

import (
	"errors"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

const (
	testOwner      = "6f0e8f9a-2b1c-4d3e-8f70-1a2b3c4d5e6f"
	testOtherOwner = "0b9c1a64-1111-4222-8333-444455556666"
)

// A machine's owner is recorded on the domain itself (PET-76), in its
// persistent definition, before it first starts.
func TestCreateRecordsTheOwnerBeforeTheFirstStart(t *testing.T) {
	p, f := newFakeProvider(t)
	spec := testSpec()
	spec.OwnerUID = testOwner

	var startedWithout bool
	f.hook = func(method string) {
		if method == "DomainCreate" {
			f.mu.Lock()
			if d := f.domains[spec.Name]; d != nil && d.metadata[ownerNamespace] == "" {
				startedWithout = true
			}
			f.mu.Unlock()
		}
	}
	state, err := p.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if startedWithout {
		t.Error("the domain started before its owner was recorded")
	}
	if state.Owner != testOwner {
		t.Errorf("Create reports owner %q, want %q", state.Owner, testOwner)
	}
	got, err := p.Get(t.Context(), state.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Owner != testOwner {
		t.Errorf("Get reads owner %q, want %q", got.Owner, testOwner)
	}
}

// A domain made before owners were recorded has none, and that reads as
// empty rather than failing every lookup of it.
func TestAMachineWithNoOwnerRecordReadsAsUnowned(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("old-machine", "<domain/>", true)

	state, err := p.Get(t.Context(), formatUUID(d.dom.UUID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state.Owner != "" {
		t.Errorf("owner %q, want none", state.Owner)
	}
}

func TestClaimOwnerSetsOnlyWhenTheExpectedOwnerIsRecorded(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("moved-machine", "<domain/>", true)
	id := formatUUID(d.dom.UUID)

	// From none: the migration of a machine made before owners were recorded.
	if err := p.ClaimOwner(t.Context(), id, "", testOwner); err != nil {
		t.Fatalf("claim from none: %v", err)
	}
	// A stale expectation changes nothing.
	err := p.ClaimOwner(t.Context(), id, "", testOtherOwner)
	if !errors.Is(err, providers.ErrOwnerMismatch) || !errors.Is(err, providers.ErrTerminal) {
		t.Fatalf("claim with a stale expectation: %v, want ErrOwnerMismatch (terminal)", err)
	}
	if state, _ := p.Get(t.Context(), id); state.Owner != testOwner {
		t.Fatalf("a refused claim changed the owner to %q", state.Owner)
	}
	// The current owner hands over: the move.
	if err := p.ClaimOwner(t.Context(), id, testOwner, testOtherOwner); err != nil {
		t.Fatalf("hand over: %v", err)
	}
	if state, _ := p.Get(t.Context(), id); state.Owner != testOtherOwner {
		t.Errorf("owner %q after the hand-over, want %q", state.Owner, testOtherOwner)
	}
}

func TestClaimOwnerOfAMissingMachineIsNotFound(t *testing.T) {
	p, _ := newFakeProvider(t)
	err := p.ClaimOwner(t.Context(), "00000000-0000-0000-0000-0000000000aa", "", testOwner)
	if !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// A record in Hydra's namespace that Hydra did not write is not guessed at:
// the owner decides who may delete the domain.
func TestAnUnreadableOwnerRecordIsTerminal(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("tampered", "<domain/>", true)
	d.metadata[ownerNamespace] = `<hydra:owner xmlns:hydra="` + ownerNamespace + `"/>`

	_, err := p.Get(t.Context(), formatUUID(d.dom.UUID))
	if !errors.Is(err, providers.ErrTerminal) {
		t.Fatalf("got %v, want a terminal error", err)
	}
}

// Any other failure reading the record is an ordinary error, not "unowned":
// reading it as unowned would let a machine be claimed from nobody.
func TestAFailedOwnerReadIsNotUnowned(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("flaky", "<domain/>", true)
	f.errs["DomainGetMetadata"] = lvErr(golibvirt.ErrInternalError)

	if _, err := p.Get(t.Context(), formatUUID(d.dom.UUID)); err == nil {
		t.Fatal("Get succeeded although the owner could not be read")
	}
	if err := p.ClaimOwner(t.Context(), formatUUID(d.dom.UUID), "", testOwner); err == nil ||
		errors.Is(err, providers.ErrOwnerMismatch) {
		t.Fatalf("claim: %v, want the read failure", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := d.metadata[ownerNamespace]; ok {
		t.Error("an owner was recorded although the current one could not be read")
	}
}
