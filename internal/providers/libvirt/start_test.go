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

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// fakeStart records which of startVerified's steps ran.
type fakeStart struct {
	active    bool
	activeErr error
	xml       string
	xmlErr    error
	startErr  error

	readXML bool
	started bool
}

func (f *fakeStart) steps() startSteps {
	return startSteps{
		isActive: func() (bool, error) { return f.active, f.activeErr },
		readXML: func() (string, error) {
			f.readXML = true
			return f.xml, f.xmlErr
		},
		start: func() error {
			f.started = true
			return f.startErr
		},
	}
}

// A domain from domainXML, as libvirt would store it: every port hotplug off.
func goodDomainXML() string {
	return domainXML(testSpec(), "/var/lib/libvirt/k8s-workers/worker-1.qcow2", "")
}

func TestStartVerifiedStartsADomainWhosePortsPass(t *testing.T) {
	f := &fakeStart{xml: goodDomainXML()}
	if err := startVerified("worker-1", f.steps()); err != nil {
		t.Fatalf("startVerified: %v", err)
	}
	if !f.readXML || !f.started {
		t.Errorf("read back = %v, started = %v; want both", f.readXML, f.started)
	}
}

// The case the check exists for: libvirt added hotplug-on ports. The domain
// must not start, and the refusal is terminal because a retry reads back the
// same domain.
func TestStartVerifiedRefusesHotplugOnPortsTerminally(t *testing.T) {
	f := &fakeStart{xml: libvirtShortOfRootPorts}
	err := startVerified("pet55-probe-short", f.steps())
	if err == nil {
		t.Fatal("a domain with hotplug-on root ports was started")
	}
	if f.started {
		t.Error("start was called on a domain that failed the check")
	}
	if !errors.Is(err, providers.ErrTerminal) {
		t.Errorf("err = %v, want it to wrap ErrTerminal", err)
	}
}

// A read-back that fails must not count as a pass. This is the adopt-path hole:
// a connection lost here leaves an unchecked domain for the next reconcile, and
// that reconcile must check it again rather than start it.
func TestStartVerifiedFailsClosedWhenTheReadBackFails(t *testing.T) {
	for name, f := range map[string]*fakeStart{
		// Valid XML alongside the error, so only the error itself can refuse it.
		"RPC error":    {xml: goodDomainXML(), xmlErr: errors.New("connection reset")},
		"unparseable":  {xml: "<domain><devices>"},
		"empty answer": {xml: ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := startVerified("worker-1", f.steps())
			if err == nil || f.started {
				t.Fatalf("err = %v, started = %v; want an error and no start", err, f.started)
			}
			// Transient, so the next reconcile retries -- and checks again.
			if errors.Is(err, providers.ErrTerminal) {
				t.Errorf("err = %v; a failed read-back is not terminal", err)
			}
		})
	}
}

func TestStartVerifiedLeavesARunningDomainAlone(t *testing.T) {
	// Even with ports that would fail: a running domain has nothing left to
	// prevent, and the check must not stop or refuse it.
	f := &fakeStart{active: true, xml: libvirtShortOfRootPorts}
	if err := startVerified("worker-1", f.steps()); err != nil {
		t.Fatalf("startVerified on a running domain: %v", err)
	}
	if f.readXML || f.started {
		t.Errorf("read back = %v, started = %v; want neither", f.readXML, f.started)
	}
}

func TestStartVerifiedReportsActiveCheckAndStartFailures(t *testing.T) {
	f := &fakeStart{activeErr: errors.New("timeout")}
	if err := startVerified("worker-1", f.steps()); err == nil || f.readXML || f.started {
		t.Errorf("isActive failure: err = %v, read back = %v, started = %v; want an error and nothing else",
			err, f.readXML, f.started)
	}

	f = &fakeStart{xml: goodDomainXML(), startErr: errors.New("qemu exited")}
	if err := startVerified("worker-1", f.steps()); err == nil {
		t.Error("a failed start was reported as success")
	}
}
