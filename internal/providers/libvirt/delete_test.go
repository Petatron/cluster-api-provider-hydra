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
	"encoding/xml"
	"errors"
	"slices"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

const operatorVolume = "postgres-data.qcow2"

// seedMachine puts testSpec's machine, running, in pool as Create would have
// left it -- root disk, cloud-init image and domain -- plus any extra disk
// paths attached to the domain by hand.
func seedMachine(t *testing.T, f *fakeLibvirt, pool string, extraDisks ...string) *fakeDomain {
	t.Helper()
	spec := testSpec()
	fp := f.pools[pool]
	fp.vols[rootVolumeName(spec.Name)] = &fakeVol{}
	fp.vols[cidataVolumeName(spec.Name)] = &fakeVol{}

	def := parseDomain(t, domainXML(spec, fp.dir+"/"+rootVolumeName(spec.Name), fp.dir+"/"+cidataVolumeName(spec.Name)))
	for _, p := range extraDisks {
		def.Devices.Disks = append(def.Devices.Disks, diskDef{Type: diskTypeFile, Source: diskSourceDef{File: p}})
	}
	// A network-backed disk has no file source, and is nothing to reclaim.
	def.Devices.Disks = append(def.Devices.Disks, diskDef{Type: "network"})
	out, err := xml.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	return f.addDomain(spec.Name, string(out), true)
}

func machineGone(f *fakeLibvirt, pool, name string) bool {
	_, defined := f.domains[name]
	return !defined && !f.hasVol(pool, rootVolumeName(name)) && !f.hasVol(pool, cidataVolumeName(name))
}

// Both volumes Hydra made go; a volume the operator attached and the shared
// base image stay. Either mistake loses data.
func TestDeleteReclaimsExactlyWhatCreateMade(t *testing.T) {
	p, f := newFakeProvider(t)
	f.pools[testPool].vols[operatorVolume] = &fakeVol{}
	d := seedMachine(t, f, testPool, f.pools[testPool].dir+"/"+operatorVolume)

	if err := p.Delete(t.Context(), formatUUID(d.dom.UUID)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !machineGone(f, testPool, "worker-1") {
		t.Error("the machine's domain or volumes survived")
	}
	if !f.hasVol(testPool, operatorVolume) {
		t.Error("a volume the operator attached by hand was destroyed")
	}
	if !f.hasVol(testPool, testImage) {
		t.Error("the shared base image was destroyed")
	}
}

// The domain is the only handle a retry has on the volumes. Undefining it first
// and then failing on storage orphans the disk permanently.
func TestDeleteRemovesStorageBeforeUndefining(t *testing.T) {
	p, f := newFakeProvider(t)
	d := seedMachine(t, f, testPool)

	if err := p.Delete(t.Context(), formatUUID(d.dom.UUID)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	undefine := slices.Index(f.calls, "DomainUndefineFlags")
	reversed := slices.Clone(f.calls)
	slices.Reverse(reversed)
	lastDelete := len(f.calls) - 1 - slices.Index(reversed, "StorageVolDelete")
	if lastDelete >= len(f.calls) || undefine < lastDelete {
		t.Errorf("calls = %v; want every volume deleted before the domain is undefined", f.calls)
	}
}

func TestDeleteKeepsTheDomainWhenItsStorageCannotBeRemoved(t *testing.T) {
	for name, failing := range map[string]string{
		"volume lookup": "StorageVolLookupByPath",
		"volume delete": "StorageVolDelete:" + rootVolumeName("worker-1"),
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			d := seedMachine(t, f, testPool)
			f.errs[failing] = errInjected

			if err := p.Delete(t.Context(), formatUUID(d.dom.UUID)); !errors.Is(err, errInjected) {
				t.Fatalf("Delete = %v, want the injected failure", err)
			}
			if _, ok := f.domains["worker-1"]; !ok {
				t.Error("the domain was undefined while its disk remained; a retry can no longer find it")
			}
		})
	}
}

// A redeploy with a different --libvirt-storage-pool must not strand a disk in
// the old one. Deletion follows the domain's own paths.
func TestDeleteReclaimsAMachineFromAPoolNoLongerConfigured(t *testing.T) {
	p, f := newFakeProvider(t)
	f.addPool("old-pool")
	d := seedMachine(t, f, "old-pool")

	if err := p.Delete(t.Context(), formatUUID(d.dom.UUID)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !machineGone(f, "old-pool", "worker-1") {
		t.Error("the machine's volumes in the previously configured pool survived")
	}
}

func TestDeleteToleratesWhatIsAlreadyGone(t *testing.T) {
	for name, setup := range map[string]func(*fakeLibvirt, *fakeDomain){
		// DomainDestroy on a stopped domain reports invalid state.
		"domain already stopped": func(_ *fakeLibvirt, d *fakeDomain) { d.active = false },
		"root disk already reclaimed": func(f *fakeLibvirt, _ *fakeDomain) {
			delete(f.pools[testPool].vols, rootVolumeName("worker-1"))
		},
		"volume vanishes mid-delete": func(f *fakeLibvirt, _ *fakeDomain) {
			f.errs["StorageVolDelete"] = lvErr(golibvirt.ErrNoStorageVol)
		},
		"domain vanishes mid-delete": func(f *fakeLibvirt, _ *fakeDomain) {
			f.errs["DomainDestroy"] = lvErr(golibvirt.ErrNoDomain)
			f.errs["DomainUndefineFlags"] = lvErr(golibvirt.ErrNoDomain)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			d := seedMachine(t, f, testPool)
			setup(f, d)

			if err := p.Delete(t.Context(), formatUUID(d.dom.UUID)); err != nil {
				t.Fatalf("Delete = %v, want success", err)
			}
		})
	}
}

func TestDeleteReportsBackendFaults(t *testing.T) {
	for name, setup := range map[string]func(*fakeLibvirt, *fakeDomain){
		"lookup fails":       func(f *fakeLibvirt, _ *fakeDomain) { f.errs["DomainLookupByUUID"] = errInjected },
		"destroy fails":      func(f *fakeLibvirt, _ *fakeDomain) { f.errs["DomainDestroy"] = errInjected },
		"domain XML fails":   func(f *fakeLibvirt, _ *fakeDomain) { f.errs["DomainGetXMLDesc"] = errInjected },
		"undefine fails":     func(f *fakeLibvirt, _ *fakeDomain) { f.errs["DomainUndefineFlags"] = errInjected },
		"domain XML garbage": func(_ *fakeLibvirt, d *fakeDomain) { d.xml = "<domain" },
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			d := seedMachine(t, f, testPool)
			setup(f, d)

			err := p.Delete(t.Context(), formatUUID(d.dom.UUID))
			if err == nil {
				t.Fatal("Delete succeeded")
			}
			if errors.Is(err, providers.ErrNotFound) || errors.Is(err, providers.ErrTerminal) {
				t.Errorf("Delete = %v; a backend fault is neither absent nor terminal", err)
			}
		})
	}
}

// Deleting an absent machine is the end state teardown wants, and reporting
// otherwise wedges the finalizer.
func TestDeleteOfAnAbsentMachineSucceeds(t *testing.T) {
	p, _ := newFakeProvider(t)
	if err := p.Delete(t.Context(), "00000000-0000-0000-0000-0000000000ff"); err != nil {
		t.Fatalf("Delete = %v, want success", err)
	}
}

// Distinct from absent: the controller falls back to deleting by name.
func TestDeleteSignalsAnIDThatCanNeverResolve(t *testing.T) {
	p, f := newFakeProvider(t)
	if err := p.Delete(t.Context(), "not-a-uuid"); !errors.Is(err, providers.ErrInvalidID) {
		t.Fatalf("Delete = %v, want ErrInvalidID", err)
	}
	if f.called("DomainLookupByUUID") != 0 {
		t.Error("an unparseable ID was sent to libvirt")
	}
}

func TestDeleteByNameReclaimsADefinedMachine(t *testing.T) {
	p, f := newFakeProvider(t)
	seedMachine(t, f, testPool)

	if err := p.DeleteByName(t.Context(), "worker-1"); err != nil {
		t.Fatalf("DeleteByName: %v", err)
	}
	if !machineGone(f, testPool, "worker-1") {
		t.Error("the machine survived")
	}
}

// Create can crash between allocating volumes and defining the domain. The
// sweep has to find both volumes in whichever pool holds them, or the
// finalizer is released over an orphaned disk.
func TestDeleteByNameSweepsDomainlessLeftoversFromEveryPool(t *testing.T) {
	p, f := newFakeProvider(t)
	old := f.addPool("old-pool", rootVolumeName("worker-1"), cidataVolumeName("worker-1"), rootVolumeName("worker-2"))

	if err := p.DeleteByName(t.Context(), "worker-1"); err != nil {
		t.Fatalf("DeleteByName: %v", err)
	}
	if f.hasVol("old-pool", rootVolumeName("worker-1")) || f.hasVol("old-pool", cidataVolumeName("worker-1")) {
		t.Errorf("leftovers survived the sweep: %v", old.vols)
	}
	if !f.hasVol("old-pool", rootVolumeName("worker-2")) || !f.hasVol(testPool, testImage) {
		t.Error("the sweep deleted a volume belonging to something else")
	}
}

func TestDeleteByNameOfNothingSucceeds(t *testing.T) {
	p, _ := newFakeProvider(t)
	if err := p.DeleteByName(t.Context(), "worker-1"); err != nil {
		t.Fatalf("DeleteByName = %v, want success", err)
	}
}

func TestDeleteByNameReportsBackendFaults(t *testing.T) {
	root := rootVolumeName("worker-1")
	for name, failing := range map[string]string{
		"domain lookup": "DomainLookupByName",
		// No fallback to the configured pool: a leftover elsewhere would be
		// missed and the finalizer released over it.
		"pool listing":  "ConnectListAllStoragePools",
		"volume lookup": "StorageVolLookupByName:" + root,
		"volume delete": "StorageVolDelete:" + root,
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			f.pools[testPool].vols[root] = &fakeVol{}
			f.errs[failing] = errInjected

			if err := p.DeleteByName(t.Context(), "worker-1"); !errors.Is(err, errInjected) {
				t.Fatalf("DeleteByName = %v, want the injected failure", err)
			}
			if !f.hasVol(testPool, root) {
				t.Error("the volume was deleted despite the failure")
			}
		})
	}
}

func TestDeleteByNameReturnsCancellationUnwrapped(t *testing.T) {
	p, f := newFakeProvider(t)
	f.stall(t, "DomainLookupByName")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := p.DeleteByName(ctx, "worker-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DeleteByName = %v, want context.DeadlineExceeded", err)
	}
}
