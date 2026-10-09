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
	"strings"
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
	// Live XML nests the clone's backing chain inside its disk. The base image
	// appears there as a <source> too, one level down.
	backing := `<backingStore type="file"><source file="` + f.pools[testPool].dir + "/" + testImage +
		`"></source></backingStore><target dev="vda"`
	desc := strings.Replace(string(out), `<target dev="vda"`, backing, 1)
	if desc == string(out) {
		t.Fatal("no root disk to attach a backing store to")
	}
	return f.addDomain(spec.Name, desc, true)
}

// listPools is the RPC the sweep and the stopped-pool check both start with.
const listPools = "ConnectListAllStoragePools"

// machineGone reports whether the machine seedMachine builds is fully reclaimed.
func machineGone(f *fakeLibvirt, pool string) bool {
	name := testSpec().Name
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
	if !machineGone(f, testPool) {
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
	if !machineGone(f, "old-pool") {
		t.Error("the machine's volumes in the previously configured pool survived")
	}
}

// A stopped pool hides its volumes: the path lookup reports not-found, which is
// indistinguishable from already reclaimed. Undefining the domain then throws
// away the only handle a retry had on both disks.
func TestDeleteKeepsTheDomainWhenItsPoolIsStopped(t *testing.T) {
	p, f := newFakeProvider(t)
	d := seedMachine(t, f, testPool)
	f.pools[testPool].active = false

	err := p.Delete(t.Context(), formatUUID(d.dom.UUID))
	if err == nil {
		t.Fatal("Delete reported success while the machine's disks were unreachable")
	}
	if !strings.Contains(err.Error(), testPool) {
		t.Errorf("Delete = %v; want it to name the stopped pool an operator has to start", err)
	}
	if errors.Is(err, providers.ErrTerminal) {
		t.Errorf("Delete = %v; a stopped pool is fixed by starting it, so this is not terminal", err)
	}
	if _, ok := f.domains["worker-1"]; !ok {
		t.Error("the domain was undefined while its disks could not be reclaimed")
	}
	if !f.hasVol(testPool, rootVolumeName("worker-1")) || !f.hasVol(testPool, cidataVolumeName("worker-1")) {
		t.Error("a volume vanished from a pool that was not running")
	}

	// Once the pool runs again, the retry finishes the job.
	f.pools[testPool].active = true
	if err := p.Delete(t.Context(), formatUUID(d.dom.UUID)); err != nil {
		t.Fatalf("Delete after the pool started = %v", err)
	}
	if !machineGone(f, testPool) || !f.hasVol(testPool, testImage) {
		t.Error("the retry did not reclaim exactly the machine")
	}
}

// Only a stopped pool that owns the disk's directory holds teardown up. One
// elsewhere on the host is no reason to keep a machine whose disks are gone.
func TestDeleteIgnoresAStoppedPoolThatDoesNotHoldTheDisk(t *testing.T) {
	p, f := newFakeProvider(t)
	d := seedMachine(t, f, testPool)
	delete(f.pools[testPool].vols, rootVolumeName("worker-1"))
	f.addPool("unrelated").active = false

	if err := p.Delete(t.Context(), formatUUID(d.dom.UUID)); err != nil {
		t.Fatalf("Delete = %v; an unrelated stopped pool must not block it", err)
	}
	if !machineGone(f, testPool) {
		t.Error("the machine was not reclaimed")
	}
}

// A miss that cannot be checked against the stopped pools is not proof of
// reclamation. Keep the domain rather than guess.
func TestDeleteKeepsTheDomainWhenStoppedPoolsCannotBeChecked(t *testing.T) {
	for name, failing := range map[string]string{
		"pool listing": listPools,
		"pool XML":     "StoragePoolGetXMLDesc",
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			d := seedMachine(t, f, testPool)
			f.pools[testPool].active = false
			f.errs[failing] = errInjected

			if err := p.Delete(t.Context(), formatUUID(d.dom.UUID)); !errors.Is(err, errInjected) {
				t.Fatalf("Delete = %v, want the injected failure", err)
			}
			if _, ok := f.domains["worker-1"]; !ok {
				t.Error("the domain was undefined without proof its disks were gone")
			}
		})
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

	if err := p.DeleteByName(t.Context(), "worker-1", ""); err != nil {
		t.Fatalf("DeleteByName: %v", err)
	}
	if !machineGone(f, testPool) {
		t.Error("the machine survived")
	}
}

// Create can crash between allocating volumes and defining the domain. The
// sweep has to find both volumes in whichever pool holds them, or the
// finalizer is released over an orphaned disk.
func TestDeleteByNameSweepsDomainlessLeftoversFromEveryPool(t *testing.T) {
	p, f := newFakeProvider(t)
	old := f.addPool("old-pool", rootVolumeName("worker-1"), cidataVolumeName("worker-1"), rootVolumeName("worker-2"))

	if err := p.DeleteByName(t.Context(), "worker-1", ""); err != nil {
		t.Fatalf("DeleteByName: %v", err)
	}
	if f.hasVol("old-pool", rootVolumeName("worker-1")) || f.hasVol("old-pool", cidataVolumeName("worker-1")) {
		t.Errorf("leftovers survived the sweep: %v", old.vols)
	}
	if !f.hasVol("old-pool", rootVolumeName("worker-2")) || !f.hasVol(testPool, testImage) {
		t.Error("the sweep deleted a volume belonging to something else")
	}
}

// A stopped pool's contents cannot be listed. When it is the machine's own
// pool, the leftover could be there, so the sweep finishes what it can see and
// then fails naming it, rather than release the finalizer over a lost disk.
func TestDeleteByNameWaitsForTheMachinesOwnStoppedPool(t *testing.T) {
	p, f := newFakeProvider(t)
	f.pools[testPool].vols[rootVolumeName("worker-1")] = &fakeVol{}
	// Sorts before testPool, so a sweep that tripped over it would never reach
	// the running pool.
	own := f.addPool("archive", cidataVolumeName("worker-1"))
	own.active = false

	err := p.DeleteByName(t.Context(), "worker-1", "archive")
	if err == nil || !strings.Contains(err.Error(), "archive") {
		t.Fatalf("DeleteByName = %v; want a failure naming the machine's stopped pool", err)
	}
	if errors.Is(err, providers.ErrTerminal) {
		t.Errorf("DeleteByName = %v; starting the pool fixes this, so it is not terminal", err)
	}
	if f.hasVol(testPool, rootVolumeName("worker-1")) {
		t.Error("the leftover in the running pool was not swept")
	}
	if !f.hasVol("archive", cidataVolumeName("worker-1")) {
		t.Error("a volume vanished from a pool that was not running")
	}

	own.active = true
	if err := p.DeleteByName(t.Context(), "worker-1", "archive"); err != nil {
		t.Fatalf("DeleteByName after the pool started = %v", err)
	}
	if f.hasVol("archive", cidataVolumeName("worker-1")) {
		t.Error("the retry did not sweep the pool once it ran")
	}
}

// The controller sweeps after every deletion. A stopped pool that is not the
// machine's must not hold that up, or one idle pool anywhere on the host keeps
// every machine in Deleting and stalls scale-down and cluster deletion.
func TestDeleteByNameSkipsSomeoneElsesStoppedPool(t *testing.T) {
	p, f := newFakeProvider(t)
	f.pools[testPool].vols[rootVolumeName("worker-1")] = &fakeVol{}
	f.addPool("archive", "unrelated.qcow2").active = false

	if err := p.DeleteByName(t.Context(), "worker-1", testPool); err != nil {
		t.Fatalf("DeleteByName = %v; an unrelated stopped pool must not block it", err)
	}
	if f.hasVol(testPool, rootVolumeName("worker-1")) {
		t.Error("the leftover in the machine's pool was not swept")
	}
	if !f.hasVol("archive", "unrelated.qcow2") {
		t.Error("the sweep touched a pool that was not running")
	}
}

// A machine whose cluster named no pool was built in the configured one, so
// that is the pool the sweep must not give up on.
func TestDeleteByNameTakesTheConfiguredPoolAsTheMachinesOwn(t *testing.T) {
	p, f := newFakeProvider(t)
	f.pools[testPool].active = false

	err := p.DeleteByName(t.Context(), "worker-1", "")
	if err == nil || !strings.Contains(err.Error(), testPool) {
		t.Fatalf("DeleteByName = %v; want a failure naming the configured pool", err)
	}

	// With no pool known at all, nothing marks a stopped pool as the one that
	// matters, and every one is skipped.
	p.cfg.StoragePool = ""
	if err := p.DeleteByName(t.Context(), "worker-1", ""); err != nil {
		t.Fatalf("DeleteByName with no pool known = %v, want success", err)
	}
}

// Not-found covers a pool undefined between the list and the lookup, which has
// taken any way of reaching its volumes with it.
func TestDeleteByNameSkipsAPoolUndefinedMidSweep(t *testing.T) {
	p, f := newFakeProvider(t)
	f.errs["StorageVolLookupByName:"+rootVolumeName("worker-1")] = lvErr(golibvirt.ErrNoStoragePool)

	if err := p.DeleteByName(t.Context(), "worker-1", ""); err != nil {
		t.Fatalf("DeleteByName = %v, want success", err)
	}
}

// The stopped pools are listed separately after the sweep. If that list
// fails, the machine's own pool cannot be ruled out.
func TestDeleteByNameFailsWhenStoppedPoolsCannotBeListed(t *testing.T) {
	p, f := newFakeProvider(t)
	lists := 0
	f.hook = func(method string) {
		if method != listPools {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if lists++; lists == 2 {
			f.errs[listPools] = errInjected
		}
	}

	if err := p.DeleteByName(t.Context(), "worker-1", ""); !errors.Is(err, errInjected) {
		t.Fatalf("DeleteByName = %v, want the injected failure", err)
	}
}

func TestDeleteByNameOfNothingSucceeds(t *testing.T) {
	p, _ := newFakeProvider(t)
	if err := p.DeleteByName(t.Context(), "worker-1", ""); err != nil {
		t.Fatalf("DeleteByName = %v, want success", err)
	}
}

func TestDeleteByNameReportsBackendFaults(t *testing.T) {
	root := rootVolumeName("worker-1")
	for name, failing := range map[string]string{
		"domain lookup": "DomainLookupByName",
		// No fallback to the configured pool: a leftover elsewhere would be
		// missed and the finalizer released over it.
		"pool listing":  listPools,
		"volume lookup": "StorageVolLookupByName:" + root,
		"volume delete": "StorageVolDelete:" + root,
	} {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			f.pools[testPool].vols[root] = &fakeVol{}
			f.errs[failing] = errInjected

			if err := p.DeleteByName(t.Context(), "worker-1", ""); !errors.Is(err, errInjected) {
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
	if err := p.DeleteByName(ctx, "worker-1", ""); err != context.DeadlineExceeded {
		t.Fatalf("DeleteByName = %v, want context.DeadlineExceeded itself", err)
	}
}
