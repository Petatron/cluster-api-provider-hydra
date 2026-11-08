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
	"reflect"
	"slices"
	"strings"
	"testing"

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
		`" id="` + testNetworkOwner.ID + `"/></metadata><bridge name="virbr7"/></network>`

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
