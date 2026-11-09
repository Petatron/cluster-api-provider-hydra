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
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

var testNetworkOwner = providers.NetworkOwner{ID: "5d6e7f80-1111-4222-8333-444455556666", Cluster: "ns/cluster"}

// ownedNetwork defines testManagedNetwork the way EnsureInfrastructure does
// for testNetworkOwner, and returns its bridge.
func ownedNetwork(t *testing.T, p *Provider, f *fakeLibvirt) string {
	t.Helper()
	spec := clusterSpec()
	n := testManagedNetwork
	n.Owner = testNetworkOwner
	spec.ManagedNetwork = &n
	if err := p.EnsureInfrastructure(t.Context(), spec); err != nil {
		t.Fatalf("EnsureInfrastructure: %v", err)
	}
	var def networkDef
	if err := xml.Unmarshal([]byte(f.networks[n.Name].xml), &def); err != nil {
		t.Fatalf("network XML: %v", err)
	}
	return def.Bridge.Name
}

// attachedDomain is a domain with one interface on the given bridge, which is
// how Hydra's machines attach to a managed network.
func attachedDomain(bridge string) string {
	return `<domain><name>guest</name><devices><interface type="bridge"><source bridge="` + bridge +
		`"/></interface></devices></domain>`
}

// The owner record goes into the same document that defines the network, in
// Hydra's namespace, so a crash cannot leave a network Hydra made unmarked.
func TestCreatedNetworkRecordsItsOwnerInTheDefinition(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)

	doc := f.networks[testManagedNetwork.Name].xml
	if !strings.Contains(doc, `xmlns="`+networkOwnerNamespace+`"`) || !strings.Contains(doc, testNetworkOwner.ID) {
		t.Fatalf("network definition carries no owner record in %s:\n%s", networkOwnerNamespace, doc)
	}
	if f.called("NetworkDefineXML") != 1 {
		t.Errorf("defined %d times, want once", f.called("NetworkDefineXML"))
	}
}

// The struct tag cannot name the constant, so this holds them together.
func TestNetworkOwnerNamespaceMatchesTheStructTag(t *testing.T) {
	field, _ := reflect.TypeFor[netMetadataDef]().FieldByName("Owner")
	if got := strings.Fields(field.Tag.Get("xml"))[0]; got != networkOwnerNamespace {
		t.Fatalf("struct tag namespace %q, constant %q", got, networkOwnerNamespace)
	}
}

// What libvirt keeps is the element in its own namespace, which may sit under
// a prefix. A record in that shape must still be recognised.
func TestDeleteNetworkReadsAPrefixedOwnerRecord(t *testing.T) {
	p, f := newFakeProvider(t)
	def := matchingNetworkDef()
	n := f.addNetwork(def.Name, def, true)
	n.xml = `<network><name>` + def.Name + `</name><metadata><hydra:owner xmlns:hydra="` + networkOwnerNamespace +
		`" id="` + testNetworkOwner.ID + `" cluster="` + testNetworkOwner.Cluster + `"/></metadata><bridge name="virbr7"/></network>`

	if err := p.DeleteNetwork(t.Context(), def.Name, testNetworkOwner); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, ok := f.networks[def.Name]; ok {
		t.Error("the network is still defined")
	}
}

func TestDeleteNetworkRemovesAnOwnedUnusedNetwork(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)

	if err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, ok := f.networks[testManagedNetwork.Name]; ok {
		t.Fatal("the network is still defined")
	}
	// Undefined before stopped: stopped first, a reconcile racing teardown could
	// find it still defined and start it again.
	calls := f.calls
	undefine, destroy := slices.Index(calls, "NetworkUndefine"), slices.Index(calls, "NetworkDestroy")
	if undefine < 0 || destroy < 0 || undefine > destroy {
		t.Errorf("calls %v: want NetworkUndefine before NetworkDestroy", calls)
	}
}

// An adopted network -- one an operator built, which carries no record -- is
// never removed, whoever asks.
func TestDeleteNetworkLeavesAnAdoptedNetwork(t *testing.T) {
	p, f := newFakeProvider(t)
	def := matchingNetworkDef()
	f.addNetwork(def.Name, def, true)

	err := p.DeleteNetwork(t.Context(), def.Name, testNetworkOwner)
	if !errors.Is(err, providers.ErrNetworkNotOwned) {
		t.Fatalf("got %v, want ErrNetworkNotOwned", err)
	}
	if _, ok := f.networks[def.Name]; !ok || f.called("NetworkUndefine")+f.called("NetworkDestroy") != 0 {
		t.Error("an adopted network was touched")
	}
}

// Another owner's network -- the same name recorded for a different cluster,
// or a cluster recreated under the same name with a new owner ID -- is left.
func TestDeleteNetworkLeavesAnotherOwnersNetwork(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)

	for _, other := range []providers.NetworkOwner{
		{ID: "0b9c1a64-1111-4222-8333-444455556666", Cluster: testNetworkOwner.Cluster},
		{},
	} {
		if err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, other); !errors.Is(err, providers.ErrNetworkNotOwned) {
			t.Fatalf("owner %+v: got %v, want ErrNetworkNotOwned", other, err)
		}
	}
	if _, ok := f.networks[testManagedNetwork.Name]; !ok {
		t.Error("another owner's network was removed")
	}
}

// A domain on the network's bridge -- running, or defined and stopped, or with
// the interface only in its persistent definition -- holds the network.
func TestDeleteNetworkWaitsForEveryDomainOnIt(t *testing.T) {
	cases := map[string]func(f *fakeLibvirt, bridge string){
		"running, by bridge": func(f *fakeLibvirt, bridge string) {
			f.addDomain("guest", attachedDomain(bridge), true)
		},
		"stopped, by bridge": func(f *fakeLibvirt, bridge string) {
			f.addDomain("guest", attachedDomain(bridge), false)
		},
		"persistent definition only": func(f *fakeLibvirt, bridge string) {
			d := f.addDomain("guest", "<domain><name>guest</name></domain>", true)
			d.inactiveXML = attachedDomain(bridge)
		},
		"by network name": func(f *fakeLibvirt, _ string) {
			f.addDomain("guest", `<domain><devices><interface type="network"><source network="`+
				testManagedNetwork.Name+`"/></interface></devices></domain>`, true)
		},
	}
	for name, attach := range cases {
		t.Run(name, func(t *testing.T) {
			p, f := newFakeProvider(t)
			bridge := ownedNetwork(t, p, f)
			attach(f, bridge)

			err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner)
			if !errors.Is(err, providers.ErrNetworkInUse) || !strings.Contains(err.Error(), "guest") {
				t.Fatalf("got %v, want ErrNetworkInUse naming the domain", err)
			}
			if _, ok := f.networks[testManagedNetwork.Name]; !ok {
				t.Error("a network in use was removed")
			}
		})
	}
}

// Gone already is done: teardown must not wedge on it.
func TestDeleteNetworkThatIsAlreadyGone(t *testing.T) {
	p, _ := newFakeProvider(t)
	if err := p.DeleteNetwork(t.Context(), "never-there", testNetworkOwner); err != nil {
		t.Fatalf("got %v, want nil", err)
	}
}

// An attempt that undefined the network and then failed to stop it leaves it
// running and transient, which libvirt refuses to undefine again. The retry must
// finish the job, not fail on the step already done -- or the cluster's
// deletion waits on it forever.
func TestDeleteNetworkFinishesAnAttemptInterruptedAfterUndefine(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)
	f.errs["NetworkDestroy"] = lvErr(golibvirt.ErrInternalError)

	if err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner); err == nil {
		t.Fatal("first attempt succeeded although the destroy failed")
	}
	if n := f.networks[testManagedNetwork.Name]; n == nil || !n.transient {
		t.Fatalf("precondition: want the network left running and transient, got %+v", n)
	}

	delete(f.errs, "NetworkDestroy")
	if err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, ok := f.networks[testManagedNetwork.Name]; ok {
		t.Error("the network is still there after the retry")
	}
}

// A stopped owned network is only undefined; there is nothing to stop.
func TestDeleteNetworkThatIsStopped(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)
	f.networks[testManagedNetwork.Name].active = false

	if err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, ok := f.networks[testManagedNetwork.Name]; ok {
		t.Error("the network is still defined")
	}
}

// A failure to look is not an answer: an unreadable domain list must not be
// taken to mean nothing uses the network.
func TestDeleteNetworkFailsWhenItCannotLook(t *testing.T) {
	for _, method := range []string{"ConnectListAllDomains", "DomainGetXMLDesc", "NetworkGetXMLDesc"} {
		t.Run(method, func(t *testing.T) {
			p, f := newFakeProvider(t)
			bridge := ownedNetwork(t, p, f)
			f.addDomain("guest", attachedDomain(bridge+"-elsewhere"), true)
			f.errs[method] = lvErr(golibvirt.ErrInternalError)

			err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner)
			if err == nil || errors.Is(err, providers.ErrNetworkNotOwned) || errors.Is(err, providers.ErrNetworkInUse) {
				t.Fatalf("got %v, want the failure itself", err)
			}
			if _, ok := f.networks[testManagedNetwork.Name]; !ok {
				t.Error("the network was removed without a complete check")
			}
		})
	}
}

// The record names the cluster as well as the ID, and both must match: the ID
// lives in an annotation and can be copied, but only an object of the same
// namespace and name is a moved cluster.
func TestDeleteNetworkRequiresTheRecordedCluster(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)

	copied := providers.NetworkOwner{ID: testNetworkOwner.ID, Cluster: "elsewhere/cluster"}
	if err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, copied); !errors.Is(err, providers.ErrNetworkNotOwned) {
		t.Fatalf("got %v, want ErrNetworkNotOwned", err)
	}
	if _, ok := f.networks[testManagedNetwork.Name]; !ok {
		t.Error("removed on a copied ID")
	}
}

// Reading the persistent definition can fail where a running one exists -- a
// network an earlier attempt undefined. The live definition is read instead,
// rather than taking the failure to mean the network is gone.
func TestDeleteNetworkFallsBackToTheRunningDefinition(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)
	f.errs["NetworkGetXMLDesc:inactive"] = lvErr(golibvirt.ErrOperationInvalid)

	if err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if _, ok := f.networks[testManagedNetwork.Name]; ok {
		t.Error("the network is still there")
	}
}

// A machine being created holds the network from resolving its bridge until
// its domain is defined, and a teardown in that window waits for it -- and
// then finds the domain, and leaves the network.
func TestDeleteNetworkWaitsForACreateThatIsAttaching(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once // blocks the first call only; the hook is never reassigned
	f.hook = func(method string) {
		if method == "DomainDefineXML" {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	spec := testSpec()
	n := testManagedNetwork
	spec.ManagedNetwork = &n
	created := make(chan error, 1)
	go func() { _, err := p.Create(t.Context(), spec); created <- err }()
	<-entered

	// Torn down through a second provider that reaches the same daemon by
	// another address: the lock is the daemon's, not the address's.
	other := &Provider{cfg: Config{RemoteAddr: "10.0.0.13:16514", StoragePool: testPool, RPCTimeout: 5 * time.Second}, lv: f}
	deleted := make(chan error, 1)
	go func() { deleted <- other.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner) }()
	select {
	case err := <-deleted:
		t.Fatalf("DeleteNetwork returned (%v) while a create was attaching to the network", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := <-created; err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := <-deleted; !errors.Is(err, providers.ErrNetworkInUse) {
		t.Fatalf("got %v, want ErrNetworkInUse once the domain was defined", err)
	}
}

// A cluster ensuring the network -- one that shares it -- waits out a teardown
// in progress instead of verifying a network about to vanish, and then defines
// it again for itself.
func TestEnsureInfrastructureWaitsForATeardownInProgress(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once // blocks the first call only; the hook is never reassigned
	f.hook = func(method string) {
		if method == "NetworkUndefine" {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	deleted := make(chan error, 1)
	go func() { deleted <- p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner) }()
	<-entered

	sharer := clusterSpec()
	n := testManagedNetwork
	n.Owner = providers.NetworkOwner{ID: "0b9c1a64-1111-4222-8333-444455556666", Cluster: "other/sharer"}
	sharer.ManagedNetwork = &n
	ensured := make(chan error, 1)
	go func() { ensured <- p.EnsureInfrastructure(t.Context(), sharer) }()
	select {
	case err := <-ensured:
		t.Fatalf("EnsureInfrastructure returned (%v) inside another cluster's teardown", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if err := <-ensured; err != nil {
		t.Fatalf("EnsureInfrastructure: %v", err)
	}
	f.mu.Lock()
	doc := f.networks[testManagedNetwork.Name]
	f.mu.Unlock()
	if doc == nil || !strings.Contains(doc.xml, n.Owner.ID) {
		t.Fatal("the sharer did not define the network again for itself")
	}
}

// Without the daemon's identity there is no lock to take, and going ahead
// without one is the race the lock exists for.
func TestDeleteNetworkFailsWithoutTheHostIdentity(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)
	p.host = ""
	f.errs["ConnectGetHostname"] = lvErr(golibvirt.ErrInternalError)

	if err := p.DeleteNetwork(t.Context(), testManagedNetwork.Name, testNetworkOwner); err == nil {
		t.Fatal("DeleteNetwork went ahead without its lock")
	}
	if _, ok := f.networks[testManagedNetwork.Name]; !ok {
		t.Error("the network was removed without the lock")
	}
}

// Waiting for the lock gives up with the caller's context, instead of waiting
// out whatever holds it.
func TestNetworkLockWaitEndsWithTheContext(t *testing.T) {
	p, f := newFakeProvider(t)
	ownedNetwork(t, p, f)

	unlock, err := p.lockNetwork(t.Context(), testManagedNetwork.Name)
	if err != nil {
		t.Fatalf("lockNetwork: %v", err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = p.DeleteNetwork(ctx, testManagedNetwork.Name, testNetworkOwner)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context's deadline", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("waited %v for a lock it should have given up on", waited)
	}
	if _, ok := f.networks[testManagedNetwork.Name]; !ok {
		t.Error("the network was touched without the lock")
	}
}
