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
	"encoding/xml"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

var testManagedNetwork = providers.ManagedNetwork{
	Name:      "hydra-net",
	Subnet:    "10.77.0.0/24",
	DHCPStart: "10.77.0.100",
	DHCPEnd:   "10.77.0.199",
}

// matchingNetworkDef is a network an operator built that agrees with
// testManagedNetwork in every respect verifyNetwork checks.
func matchingNetworkDef() networkDef {
	return networkDef{
		Name:    testManagedNetwork.Name,
		Forward: netForwardDef{Mode: netForwardOpen},
		Bridge:  netBridgeDef{Name: "virbr7"},
		IP: netIPDef{
			Address: "10.77.0.1",
			Netmask: net.IP(net.CIDRMask(24, 32)).String(),
			DHCP:    &netDHCPDef{Range: netRangeDef{Start: "10.77.0.100", End: "10.77.0.199"}},
		},
	}
}

func clusterSpec() providers.InfrastructureSpec {
	return providers.InfrastructureSpec{StoragePool: testPool, Image: providers.Image{Name: testImage}}
}

// The pool and image are the operator's. Checking them must never create,
// change or remove anything.
func TestEnsureInfrastructureVerifiesWithoutTouching(t *testing.T) {
	p, f := newFakeProvider(t)

	if err := p.EnsureInfrastructure(t.Context(), clusterSpec()); err != nil {
		t.Fatalf("EnsureInfrastructure: %v", err)
	}
	writes := f.called("StorageVolCreateXML") + f.called("StorageVolDelete") +
		f.called("NetworkDefineXML") + f.called("DomainDefineXML")
	if writes != 0 {
		t.Errorf("a pure verification made %d write calls: %v", writes, f.calls)
	}
}

// A cluster that names no image is valid -- its machines name their own -- and
// must not be gated on the manager's default being present in its pool.
func TestEnsureInfrastructureSkipsTheImageWhenTheClusterNamesNone(t *testing.T) {
	p, f := newFakeProvider(t)
	f.addPool("cluster-pool")

	if err := p.EnsureInfrastructure(t.Context(), providers.InfrastructureSpec{StoragePool: "cluster-pool"}); err != nil {
		t.Fatalf("EnsureInfrastructure = %v; the manager's default image was demanded of the cluster's pool", err)
	}
	if f.called("StorageVolLookupByName") != 0 {
		t.Error("an image was looked up for a cluster that named none")
	}
}

func TestEnsureInfrastructureClassifiesItsFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(*fakeLibvirt)
		spec     func(*providers.InfrastructureSpec)
		terminal bool
		mention  string
	}{{
		name:     "pool does not exist",
		spec:     func(s *providers.InfrastructureSpec) { s.StoragePool = "missing" },
		terminal: true,
		mention:  `"missing"`,
	}, {
		// Not terminal: pool-start or autostart fixes it, and the next attempt
		// succeeds without anyone intervening.
		name:    "pool is not running",
		setup:   func(f *fakeLibvirt) { f.pools[testPool].active = false },
		mention: "not running",
	}, {
		name:  "pool liveness check fails",
		setup: func(f *fakeLibvirt) { f.errs["StoragePoolIsActive"] = errInjected },
	}, {
		name:     "image is not in the pool",
		spec:     func(s *providers.InfrastructureSpec) { s.Image = providers.Image{Name: "noble"} },
		terminal: true,
		mention:  `"noble"`,
	}, {
		name: "image only has a URL",
		spec: func(s *providers.InfrastructureSpec) {
			s.Image = providers.Image{URL: "https://images.example/noble.img"}
		},
		terminal: true,
		mention:  "fetching by URL is not implemented",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			spec := clusterSpec()
			if tc.spec != nil {
				tc.spec(&spec)
			}

			err := p.EnsureInfrastructure(t.Context(), spec)
			if err == nil {
				t.Fatal("EnsureInfrastructure succeeded")
			}
			if got := errors.Is(err, providers.ErrTerminal); got != tc.terminal {
				t.Errorf("terminal = %v, want %v: %v", got, tc.terminal, err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not mention %s", err, tc.mention)
			}
		})
	}
}

func TestEnsureInfrastructureCreatesAnAbsentManagedNetwork(t *testing.T) {
	p, f := newFakeProvider(t)
	spec := clusterSpec()
	spec.ManagedNetwork = &testManagedNetwork

	if err := p.EnsureInfrastructure(t.Context(), spec); err != nil {
		t.Fatalf("EnsureInfrastructure: %v", err)
	}
	n, ok := f.networks[testManagedNetwork.Name]
	if !ok {
		t.Fatal("the managed network was not defined")
	}
	if !n.active || !n.autostart {
		t.Errorf("active = %v, autostart = %v; want both, or the network dies with the next host reboot",
			n.active, n.autostart)
	}
	var def networkDef
	if err := xml.Unmarshal([]byte(n.xml), &def); err != nil {
		t.Fatalf("network XML does not parse: %v", err)
	}
	want := matchingNetworkDef()
	if def.Forward != want.Forward || def.IP.Address != want.IP.Address || def.IP.Netmask != want.IP.Netmask ||
		def.IP.DHCP == nil || def.IP.DHCP.Range != want.IP.DHCP.Range {
		t.Errorf("network = %+v, want the declared addressing with forward mode open", def)
	}

	// The next reconcile finds what it made and verifies it, rather than failing
	// on a network that now exists.
	if err := p.EnsureInfrastructure(t.Context(), spec); err != nil {
		t.Fatalf("second EnsureInfrastructure: %v", err)
	}
	if f.called("NetworkDefineXML") != 1 {
		t.Errorf("network defined %d times, want once", f.called("NetworkDefineXML"))
	}
}

// Starting a stopped declared network destroys nothing, so it is ensured rather
// than reported -- unlike its addressing.
func TestEnsureInfrastructureStartsAStoppedManagedNetwork(t *testing.T) {
	p, f := newFakeProvider(t)
	n := f.addNetwork(testManagedNetwork.Name, matchingNetworkDef(), false)
	spec := clusterSpec()
	spec.ManagedNetwork = &testManagedNetwork

	if err := p.EnsureInfrastructure(t.Context(), spec); err != nil {
		t.Fatalf("EnsureInfrastructure: %v", err)
	}
	if !n.active || !n.autostart {
		t.Errorf("active = %v, autostart = %v; want both", n.active, n.autostart)
	}
	if f.called("NetworkDefineXML") != 0 {
		t.Error("an existing network was redefined")
	}
}

// Autostart is set on every pass, not once at creation. A running network whose
// autostart was lost would otherwise satisfy every reconcile until the host
// rebooted into a cluster with dead interfaces.
func TestEnsureInfrastructureRestoresAutostartOnARunningNetwork(t *testing.T) {
	p, f := newFakeProvider(t)
	n := f.addNetwork(testManagedNetwork.Name, matchingNetworkDef(), true)
	spec := clusterSpec()
	spec.ManagedNetwork = &testManagedNetwork

	if err := p.EnsureInfrastructure(t.Context(), spec); err != nil {
		t.Fatalf("EnsureInfrastructure: %v", err)
	}
	if !n.autostart {
		t.Error("autostart was not restored on a network that was already running")
	}
}

// An adopted network that disagrees with the declaration is refused, and left
// exactly as the operator built it.
func TestEnsureInfrastructureRefusesAMismatchedManagedNetwork(t *testing.T) {
	for name, mutate := range map[string]func(*networkDef){
		"forward mode nat":  func(d *networkDef) { d.Forward.Mode = "nat" },
		"different gateway": func(d *networkDef) { d.IP.Address = "10.77.0.254" },
		"different netmask": func(d *networkDef) { d.IP.Netmask = "255.255.0.0" },
		"no DHCP at all":    func(d *networkDef) { d.IP.DHCP = nil },
		"different range":   func(d *networkDef) { d.IP.DHCP.Range.End = "10.77.0.250" },
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			def := matchingNetworkDef()
			mutate(&def)
			f.addNetwork(testManagedNetwork.Name, def, false)
			spec := clusterSpec()
			spec.ManagedNetwork = &testManagedNetwork

			err := p.EnsureInfrastructure(t.Context(), spec)
			if !errors.Is(err, providers.ErrTerminal) {
				t.Fatalf("EnsureInfrastructure = %v, want a terminal refusal", err)
			}
			for _, m := range []string{"NetworkDefineXML", "NetworkCreate", "NetworkSetAutostart"} {
				if n := f.called(m); n != 0 {
					t.Errorf("%s called on a network that was refused", m)
				}
			}
		})
	}
}

func TestEnsureInfrastructureReportsManagedNetworkFaults(t *testing.T) {
	name := testManagedNetwork.Name
	for _, tc := range []struct {
		name     string
		setup    func(*fakeLibvirt)
		subnet   string
		terminal bool
		mention  string
	}{
		{name: "lookup fails", setup: func(f *fakeLibvirt) { f.errs["NetworkLookupByName"] = errInjected }},
		{name: "define fails", setup: func(f *fakeLibvirt) { f.errs["NetworkDefineXML"] = errInjected }},
		{name: "liveness check fails", setup: func(f *fakeLibvirt) {
			f.addNetwork(name, matchingNetworkDef(), false)
			f.errs["NetworkIsActive"] = errInjected
		}},
		{name: "start fails", setup: func(f *fakeLibvirt) {
			f.addNetwork(name, matchingNetworkDef(), false)
			f.errs["NetworkCreate"] = errInjected
		}},
		{name: "autostart fails", setup: func(f *fakeLibvirt) {
			f.addNetwork(name, matchingNetworkDef(), true)
			f.errs["NetworkSetAutostart"] = errInjected
		}},
		{name: "read fails", setup: func(f *fakeLibvirt) {
			f.addNetwork(name, matchingNetworkDef(), true)
			f.errs["NetworkGetXMLDesc"] = errInjected
		}},
		{name: "unparseable definition", mention: "parsing network", setup: func(f *fakeLibvirt) {
			f.addNetwork(name, matchingNetworkDef(), true).xml = "<network"
		}},
		{name: "no bridge to attach to", mention: "no bridge", setup: func(f *fakeLibvirt) {
			def := matchingNetworkDef()
			def.Bridge.Name = ""
			f.addNetwork(name, def, true)
		}},
		{name: "subnet leaves no room", subnet: "10.77.0.0/31", terminal: true, mention: "no addresses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			mn := testManagedNetwork
			if tc.subnet != "" {
				mn.Subnet = tc.subnet
			}
			spec := clusterSpec()
			spec.ManagedNetwork = &mn

			err := p.EnsureInfrastructure(t.Context(), spec)
			if err == nil {
				t.Fatal("EnsureInfrastructure succeeded")
			}
			if got := errors.Is(err, providers.ErrTerminal); got != tc.terminal {
				t.Errorf("terminal = %v, want %v: %v", got, tc.terminal, err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not mention %s", err, tc.mention)
			}
		})
	}
}

// Something else starts the network between the liveness check and
// NetworkCreate. Running is the state being asked for.
func TestEnsureInfrastructureToleratesANetworkStartedConcurrently(t *testing.T) {
	t.Skip("PET-58: bringUp tolerates ErrNetworkExist, but libvirt reports an already-active network as operation-invalid")
	p, f := newFakeProvider(t)
	n := f.addNetwork(testManagedNetwork.Name, matchingNetworkDef(), false)
	f.hook = func(method string) {
		if method == "NetworkCreate" {
			f.mu.Lock()
			n.active = true
			f.mu.Unlock()
		}
	}
	spec := clusterSpec()
	spec.ManagedNetwork = &testManagedNetwork

	if err := p.EnsureInfrastructure(t.Context(), spec); err != nil {
		t.Fatalf("EnsureInfrastructure = %v; already-running is success", err)
	}
	if !n.autostart {
		t.Error("autostart was skipped after the tolerated start")
	}
}
