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
	"context"
	"errors"
	"slices"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

func ifaceWith(addrs ...string) []golibvirt.DomainInterface {
	iface := golibvirt.DomainInterface{Name: "enp1s0"}
	for _, a := range addrs {
		iface.Addrs = append(iface.Addrs, golibvirt.DomainIPAddr{Addr: a})
	}
	return []golibvirt.DomainInterface{iface}
}

func internalIPs(state *providers.MachineState) []string {
	var out []string
	for _, a := range state.Addresses {
		if a.Type == providers.AddressTypeInternalIP {
			out = append(out, a.Address)
		}
	}
	return out
}

func TestGetReportsTheDomain(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("worker-1", goodDomainXML(), true)
	d.addrs[addrSourceAgent] = ifaceWith("192.168.15.42")

	state, err := p.Get(t.Context(), formatUUID(d.dom.UUID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state.ID != formatUUID(d.dom.UUID) || state.Name != d.dom.Name || !state.Ready {
		t.Errorf("state = %+v, want the running domain", state)
	}
	if got := internalIPs(state); !slices.Equal(got, []string{"192.168.15.42"}) {
		t.Errorf("InternalIPs = %v, want the guest agent's address", got)
	}
}

func TestGetReportsAStoppedDomainNotReady(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("worker-1", goodDomainXML(), false)

	state, err := p.Get(t.Context(), formatUUID(d.dom.UUID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if state.Ready {
		t.Error("a shut-off domain was reported ready")
	}
}

// The guest agent first, so statically addressed guests report something; then
// the lease table. Loopback and link-local are never an answer, and a source
// offering only those must not stop the search.
func TestAddressesPreferTheAgentAndFallBackToLeases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent []golibvirt.DomainInterface
		lease []golibvirt.DomainInterface
		want  []string
	}{
		{"agent answers", ifaceWith("10.0.0.5", "127.0.0.1", "fe80::1"), ifaceWith("10.0.0.9"), []string{"10.0.0.5"}},
		{"no agent", nil, ifaceWith("10.0.0.9"), []string{"10.0.0.9"}},
		{"agent sees only loopback", ifaceWith("127.0.0.1", "::1"), ifaceWith("10.0.0.9"), []string{"10.0.0.9"}},
		{"nothing yet", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			d := f.addDomain("worker-1", goodDomainXML(), true)
			if tc.agent != nil {
				d.addrs[addrSourceAgent] = tc.agent
			}
			if tc.lease != nil {
				d.addrs[addrSourceLease] = tc.lease
			}

			state, err := p.Get(t.Context(), formatUUID(d.dom.UUID))
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got := internalIPs(state); !slices.Equal(got, tc.want) {
				t.Errorf("InternalIPs = %v, want %v", got, tc.want)
			}
			// The hostname is always there, and always last.
			last := state.Addresses[len(state.Addresses)-1]
			if last.Type != providers.AddressTypeHostname || last.Address != d.dom.Name {
				t.Errorf("addresses = %+v, want the domain name last", state.Addresses)
			}
		})
	}
}

// An unavailable source is ordinary; a cancelled call is not, and reporting it
// as "no addresses yet" would hide that it never completed.
func TestAddressesReportCancellation(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("worker-1", goodDomainXML(), true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.errs["DomainInterfaceAddresses"] = errInjected
	f.hook = func(method string) {
		if method == "DomainInterfaceAddresses" {
			cancel()
		}
	}

	if _, err := p.Get(ctx, formatUUID(d.dom.UUID)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get = %v, want context.Canceled", err)
	}
}

func TestGetClassifiesFailures(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("worker-1", goodDomainXML(), true)

	if _, err := p.Get(t.Context(), "hydra-not-a-uuid"); !errors.Is(err, providers.ErrInvalidID) {
		t.Errorf("Get(garbage) = %v, want ErrInvalidID", err)
	}
	if _, err := p.Get(t.Context(), "00000000-0000-0000-0000-0000000000ff"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}

	f.errs["DomainGetState"] = errInjected
	if _, err := p.Get(t.Context(), formatUUID(d.dom.UUID)); !errors.Is(err, errInjected) {
		t.Errorf("Get with a failing state call = %v, want the failure", err)
	}

	f.errs["DomainLookupByUUID"] = errInjected
	_, err := p.Get(t.Context(), formatUUID(d.dom.UUID))
	if !errors.Is(err, errInjected) || errors.Is(err, providers.ErrNotFound) {
		t.Errorf("Get with a failing lookup = %v, want the failure and not ErrNotFound", err)
	}
}

func TestFindByName(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("worker-1", goodDomainXML(), true)

	state, err := p.FindByName(t.Context(), "worker-1")
	if err != nil || state.ID != formatUUID(d.dom.UUID) {
		t.Fatalf("FindByName = %+v, %v; want the domain", state, err)
	}
	if _, err := p.FindByName(t.Context(), "worker-2"); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("FindByName(absent) = %v, want ErrNotFound", err)
	}

	// Not ErrNotFound: deletion would release the finalizer over a machine that
	// may well exist.
	f.errs["DomainLookupByName"] = errInjected
	if _, err := p.FindByName(t.Context(), "worker-1"); !errors.Is(err, errInjected) || errors.Is(err, providers.ErrNotFound) {
		t.Errorf("FindByName with a failing lookup = %v, want the failure and not ErrNotFound", err)
	}
}

func TestName(t *testing.T) {
	if got := (&Provider{}).Name(); got != "libvirt" {
		t.Errorf("Name() = %q; it is baked into every providerID and must not change", got)
	}
}
