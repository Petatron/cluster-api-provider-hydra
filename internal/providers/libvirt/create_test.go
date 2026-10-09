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
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

var errInjected = errors.New("injected failure")

func bootstrapSpec() providers.MachineSpec {
	spec := testSpec()
	spec.Hostname = spec.Name
	spec.BootstrapData = []byte("#cloud-config\nruncmd: [kubeadm join]\n")
	return spec
}

func parseDomain(t *testing.T, desc string) domainDef {
	t.Helper()
	var d domainDef
	if err := xml.Unmarshal([]byte(desc), &d); err != nil {
		t.Fatalf("domain XML does not parse: %v\n%s", err, desc)
	}
	return d
}

func diskPaths(d domainDef) []string {
	out := make([]string, 0, len(d.Devices.Disks))
	for _, disk := range d.Devices.Disks {
		out = append(out, disk.Source.File)
	}
	return out
}

func TestCreateBuildsAMachineFromNothing(t *testing.T) {
	p, f := newFakeProvider(t)
	spec := bootstrapSpec()

	state, err := p.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	pool := f.pools[testPool]
	root, cidata := rootVolumeName(spec.Name), cidataVolumeName(spec.Name)
	rootVol, ok := pool.vols[root]
	if !ok {
		t.Fatalf("no root volume %q was created", root)
	}
	var vol volumeDef
	if err := xml.Unmarshal([]byte(rootVol.xml), &vol); err != nil {
		t.Fatalf("root volume XML does not parse: %v", err)
	}
	// By path, not by name: libvirt wants a backing-store path.
	if vol.BackingSt == nil || vol.BackingSt.Path != pool.dir+"/"+testImage {
		t.Errorf("root volume backing store = %+v, want the base image's path", vol.BackingSt)
	}
	if vol.Capacity.Value != spec.DiskBytes {
		t.Errorf("root volume capacity = %d, want %d", vol.Capacity.Value, spec.DiskBytes)
	}
	cidataVol, ok := pool.vols[cidata]
	if !ok || len(cidataVol.data) == 0 {
		t.Fatalf("cloud-init volume %q was not created and uploaded", cidata)
	}

	d, ok := f.domains[spec.Name]
	if !ok {
		t.Fatal("no domain was defined")
	}
	if !d.active {
		t.Error("the domain was defined but never started")
	}
	want := []string{pool.dir + "/" + root, pool.dir + "/" + cidata}
	if got := diskPaths(parseDomain(t, d.xml)); !slices.Equal(got, want) {
		t.Errorf("domain disks = %v, want %v", got, want)
	}

	if state.ID != formatUUID(d.dom.UUID) || state.Name != spec.Name || !state.Ready {
		t.Errorf("state = %+v, want the running domain's identity", state)
	}
	last := state.Addresses[len(state.Addresses)-1]
	if last.Type != providers.AddressTypeHostname || last.Address != spec.Name {
		t.Errorf("addresses = %+v, want the domain name as a Hostname address", state.Addresses)
	}
}

func TestCreateWithoutBootstrapDataAttachesNoCloudInit(t *testing.T) {
	p, f := newFakeProvider(t)

	if _, err := p.Create(t.Context(), testSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.hasVol(testPool, cidataVolumeName("worker-1")) {
		t.Error("a cloud-init volume was created with no bootstrap data to put in it")
	}
	if got := diskPaths(parseDomain(t, f.domains["worker-1"].xml)); len(got) != 1 {
		t.Errorf("domain disks = %v, want only the root disk", got)
	}
}

// A reconcile interrupted before its providerID was persisted calls Create
// again with the same spec. A second machine there is one nothing owns.
func TestCreateIsIdempotentOnName(t *testing.T) {
	p, f := newFakeProvider(t)

	first, err := p.Create(t.Context(), bootstrapSpec())
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := p.Create(t.Context(), bootstrapSpec())
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("IDs differ across retries: %q then %q", first.ID, second.ID)
	}
	if len(f.domains) != 1 || f.called("DomainDefineXML") != 1 || f.called("StorageVolCreateXML") != 2 {
		t.Errorf("domains = %d, defines = %d, volume creates = %d; want one machine built once",
			len(f.domains), f.called("DomainDefineXML"), f.called("StorageVolCreateXML"))
	}
}

// Adoption must depend on nothing but the name: by the time a crashed reconcile
// retries, the bootstrap Secret may have been rotated away.
func TestCreateAdoptsARunningDomainByNameAlone(t *testing.T) {
	p, f := newFakeProvider(t)
	f.addDomain("worker-1", goodDomainXML(), true)

	state, err := p.Create(t.Context(), providers.MachineSpec{Name: testSpec().Name})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !state.Ready {
		t.Error("an adopted running domain was reported not ready")
	}
	if n := f.called("StorageVolCreateXML") + f.called("DomainCreate"); n != 0 {
		t.Errorf("adopting a running domain made %d create calls, want none", n)
	}
}

// Nothing else in the reconcile loop ever starts a domain, so one that was
// defined and then interrupted would be polled forever if adoption skipped it.
func TestCreateStartsAnAdoptedDomainThatNeverStarted(t *testing.T) {
	p, f := newFakeProvider(t)
	f.addDomain("worker-1", goodDomainXML(), false)

	state, err := p.Create(t.Context(), testSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !f.domains["worker-1"].active || !state.Ready {
		t.Error("the adopted domain was not started")
	}
	if f.called("StorageVolCreateXML") != 0 {
		t.Error("adoption created volumes for a domain that already had them")
	}
}

func TestCreateRefusesToStartAnAdoptedDomainWithHotplugPorts(t *testing.T) {
	p, f := newFakeProvider(t)
	f.addDomain("worker-1", libvirtShortOfRootPorts, false)

	_, err := p.Create(t.Context(), testSpec())
	if !errors.Is(err, providers.ErrTerminal) {
		t.Fatalf("Create = %v, want a terminal refusal", err)
	}
	d, ok := f.domains["worker-1"]
	if !ok {
		t.Fatal("the refused domain was undefined; reclaiming it is Delete's job")
	}
	if d.active {
		t.Error("the refused domain was started")
	}
}

// The fresh path: libvirt stores something other than what was defined. The
// refusal leaves the domain and both volumes for Delete, which finds them by
// name.
func TestCreateRefusesAFreshDomainLibvirtAddedHotplugPortsTo(t *testing.T) {
	p, f := newFakeProvider(t)
	f.hook = func(method string) {
		if method != "DomainIsActive" {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if d, ok := f.domains["worker-1"]; ok {
			d.xml = libvirtShortOfRootPorts
		}
	}

	_, err := p.Create(t.Context(), bootstrapSpec())
	if !errors.Is(err, providers.ErrTerminal) {
		t.Fatalf("Create = %v, want a terminal refusal", err)
	}
	if d, ok := f.domains["worker-1"]; !ok || d.active {
		t.Errorf("domain defined = %v; want it left defined and never started", ok)
	}
	if !f.hasVol(testPool, rootVolumeName("worker-1")) || !f.hasVol(testPool, cidataVolumeName("worker-1")) {
		t.Error("a refusal deleted volumes; nothing on this path may clean up")
	}
}

// A root disk left by an attempt that crashed before defining the domain is a
// valid clone and is adopted. Treating it as fatal would fail every retry.
func TestCreateAdoptsALeftoverRootVolume(t *testing.T) {
	p, f := newFakeProvider(t)
	root := rootVolumeName("worker-1")
	f.pools[testPool].vols[root] = &fakeVol{xml: "from a previous attempt"}

	if _, err := p.Create(t.Context(), testSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := f.pools[testPool].vols[root].xml; got != "from a previous attempt" {
		t.Error("the leftover root volume was replaced rather than adopted")
	}
}

// The opposite of the root disk: a leftover cloud-init image may be truncated,
// and a truncated one boots a machine that configures nothing.
func TestCreateReplacesALeftoverCloudInitVolume(t *testing.T) {
	p, f := newFakeProvider(t)
	cidata := cidataVolumeName("worker-1")
	f.pools[testPool].vols[cidata] = &fakeVol{data: []byte("truncated")}

	if _, err := p.Create(t.Context(), bootstrapSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := f.pools[testPool].vols[cidata].data
	if bytes.Equal(got, []byte("truncated")) || len(got) == 0 {
		t.Errorf("cloud-init volume holds %d bytes of the leftover, want a fresh image", len(got))
	}
}

func TestCreateReportsAFailureToReplaceALeftoverCloudInitVolume(t *testing.T) {
	cidata := cidataVolumeName("worker-1")
	for name, setup := range map[string]func(*fakeLibvirt){
		"delete fails": func(f *fakeLibvirt) {
			f.errs["StorageVolDelete:"+cidata] = errInjected
		},
		"recreate fails": func(f *fakeLibvirt) {
			f.hook = func(method string) {
				if method == "StorageVolDelete" {
					f.mu.Lock()
					f.errs["StorageVolCreateXML:"+cidata] = errInjected
					f.mu.Unlock()
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			f.pools[testPool].vols[cidata] = &fakeVol{data: []byte("truncated")}
			setup(f)

			_, err := p.Create(t.Context(), bootstrapSpec())
			if !errors.Is(err, errInjected) {
				t.Fatalf("Create = %v, want the injected failure", err)
			}
			if len(f.domains) != 0 {
				t.Error("a domain was defined without its cloud-init image")
			}
			if f.hasVol(testPool, rootVolumeName("worker-1")) {
				t.Error("the root volume was left behind")
			}
		})
	}
}

// Every failure after the root volume exists and before the domain is defined
// must take that volume, and the cloud-init image, back out -- and never the
// shared base image underneath. After the domain is defined, Delete owns them.
func TestCreateRollsBackItsVolumesWhenALaterStepFails(t *testing.T) {
	root, cidata := rootVolumeName("worker-1"), cidataVolumeName("worker-1")
	for name, failing := range map[string]string{
		"cloud-init upload":    "StorageVolUpload",
		"root volume path":     "StorageVolGetPath:" + root,
		"cloud-init path":      "StorageVolGetPath:" + cidata,
		"domain definition":    "DomainDefineXML",
		"managed network read": "NetworkLookupByName",
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			f.addNetwork(testManagedNetwork.Name, matchingNetworkDef(), true)
			f.errs[failing] = errInjected
			spec := bootstrapSpec()
			spec.ManagedNetwork = &testManagedNetwork

			_, err := p.Create(t.Context(), spec)
			if !errors.Is(err, errInjected) {
				t.Fatalf("Create = %v, want the injected failure", err)
			}
			if errors.Is(err, providers.ErrTerminal) {
				t.Errorf("Create = %v; a backend fault is not terminal", err)
			}
			if f.hasVol(testPool, root) || f.hasVol(testPool, cidata) {
				t.Errorf("volumes left behind: root = %v, cloud-init = %v",
					f.hasVol(testPool, root), f.hasVol(testPool, cidata))
			}
			if !f.hasVol(testPool, testImage) {
				t.Fatal("rollback deleted the shared base image")
			}
			if len(f.domains) != 0 {
				t.Error("a domain was left defined")
			}
		})
	}
}

func TestCreateRollsBackWhenTheManagedNetworkIsTheWrongKind(t *testing.T) {
	p, f := newFakeProvider(t)
	def := matchingNetworkDef()
	def.Forward.Mode = "nat"
	f.addNetwork(testManagedNetwork.Name, def, true)
	spec := bootstrapSpec()
	spec.ManagedNetwork = &testManagedNetwork

	_, err := p.Create(t.Context(), spec)
	if !errors.Is(err, providers.ErrTerminal) {
		t.Fatalf("Create = %v, want a terminal refusal", err)
	}
	if f.hasVol(testPool, rootVolumeName("worker-1")) || f.hasVol(testPool, cidataVolumeName("worker-1")) {
		t.Error("volumes were left behind")
	}
}

// The managed network is attached in addition to the site network, which is
// what gives the machine internet access at boot.
func TestCreateAttachesTheManagedNetworkAlongsideTheSiteNetwork(t *testing.T) {
	p, f := newFakeProvider(t)
	f.addNetwork(testManagedNetwork.Name, matchingNetworkDef(), true)
	spec := testSpec()
	spec.ManagedNetwork = &testManagedNetwork

	if _, err := p.Create(t.Context(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ifaces := parseDomain(t, f.domains["worker-1"].xml).Devices.Interfaces
	bridges := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		bridges = append(bridges, iface.Source.Bridge)
	}
	if want := []string{testSpec().Networks[0].Name, matchingNetworkDef().Bridge.Name}; !slices.Equal(bridges, want) {
		t.Errorf("interfaces = %v, want %v", bridges, want)
	}
}

// Whether a failure is terminal decides between ProvisioningFailed, which an
// operator or MachineHealthCheck acts on, and a quiet retry. Each case is
// pinned in both directions.
func TestCreateClassifiesItsFailures(t *testing.T) {
	root, cidata := rootVolumeName("worker-1"), cidataVolumeName("worker-1")
	for _, tc := range []struct {
		name     string
		setup    func(*Provider, *fakeLibvirt)
		spec     func(*providers.MachineSpec)
		terminal bool
		mention  string
	}{{
		name:     "no pool named anywhere",
		setup:    func(p *Provider, _ *fakeLibvirt) { p.cfg.StoragePool = "" },
		terminal: true,
		mention:  "--libvirt-storage-pool",
	}, {
		name:     "pool does not exist",
		spec:     func(s *providers.MachineSpec) { s.StoragePool = "missing" },
		terminal: true,
		mention:  `"missing"`,
	}, {
		name:  "pool lookup fails",
		setup: func(_ *Provider, f *fakeLibvirt) { f.errs["StoragePoolLookupByName"] = errInjected },
	}, {
		// Must name the pool actually queried, not the configured one.
		name:  "image only has a URL",
		setup: func(_ *Provider, f *fakeLibvirt) { f.addPool("cluster-pool") },
		spec: func(s *providers.MachineSpec) {
			s.StoragePool = "cluster-pool"
			s.Image = providers.Image{URL: "https://images.example/noble.img"}
		},
		terminal: true,
		mention:  `"cluster-pool"`,
	}, {
		name:     "no image named anywhere",
		setup:    func(p *Provider, _ *fakeLibvirt) { p.cfg.BaseImage = "" },
		spec:     func(s *providers.MachineSpec) { s.Image = providers.Image{} },
		terminal: true,
		mention:  "no image specified",
	}, {
		name:     "image is not in the pool",
		spec:     func(s *providers.MachineSpec) { s.Image = providers.Image{Name: "noble"} },
		terminal: true,
		mention:  `"noble"`,
	}, {
		name:  "image lookup fails",
		setup: func(_ *Provider, f *fakeLibvirt) { f.errs["StorageVolLookupByName:"+testImage] = errInjected },
	}, {
		name:  "image path fails",
		setup: func(_ *Provider, f *fakeLibvirt) { f.errs["StorageVolGetPath:"+testImage] = errInjected },
	}, {
		name:  "root volume creation fails",
		setup: func(_ *Provider, f *fakeLibvirt) { f.errs["StorageVolCreateXML:"+root] = errInjected },
	}, {
		name:  "cloud-init volume creation fails",
		setup: func(_ *Provider, f *fakeLibvirt) { f.errs["StorageVolCreateXML:"+cidata] = errInjected },
		spec:  func(s *providers.MachineSpec) { s.BootstrapData = []byte("#cloud-config\n") },
	}, {
		name:     "bootstrap data too large to render",
		spec:     func(s *providers.MachineSpec) { s.BootstrapData = bytes.Repeat([]byte("x"), 1<<20+1) },
		terminal: true,
		mention:  "rendering cloud-init",
	}, {
		name:  "domain lookup fails",
		setup: func(_ *Provider, f *fakeLibvirt) { f.errs["DomainLookupByName"] = errInjected },
	}, {
		name: "managed network subnet is not CIDR",
		spec: func(s *providers.MachineSpec) {
			s.ManagedNetwork = &providers.ManagedNetwork{Name: "n", Subnet: "nope"}
		},
		terminal: true,
		mention:  `"nope"`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			if tc.setup != nil {
				tc.setup(p, f)
			}
			spec := testSpec()
			if tc.spec != nil {
				tc.spec(&spec)
			}

			_, err := p.Create(t.Context(), spec)
			if err == nil {
				t.Fatal("Create succeeded")
			}
			if got := errors.Is(err, providers.ErrTerminal); got != tc.terminal {
				t.Errorf("terminal = %v, want %v: %v", got, tc.terminal, err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not mention %s", err, tc.mention)
			}
			if f.hasVol(testPool, root) || len(f.domains) != 0 {
				t.Error("a failed Create left a root volume or a domain behind")
			}
		})
	}
}

// Cancellation reaches the caller as itself, not wrapped into a lookup error:
// the controller distinguishes the two.
func TestCreateReturnsCancellationUnwrapped(t *testing.T) {
	p, f := newFakeProvider(t)
	f.stall(t, "DomainLookupByName")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Create(ctx, testSpec()); err != context.DeadlineExceeded {
		t.Fatalf("Create = %v, want context.DeadlineExceeded itself", err)
	}
}

// Staging the image needs a writable temporary directory. A full or missing one
// fails now and works later, so it must not raise a terminal condition.
func TestCreateTreatsAnUnwritableStagingDirectoryAsTransient(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	p, f := newFakeProvider(t)

	_, err := p.Create(t.Context(), bootstrapSpec())
	if err == nil || errors.Is(err, providers.ErrTerminal) {
		t.Fatalf("Create = %v, want a transient failure", err)
	}
	if f.hasVol(testPool, rootVolumeName("worker-1")) || len(f.domains) != 0 {
		t.Error("a failed Create left a root volume or a domain behind")
	}
}
