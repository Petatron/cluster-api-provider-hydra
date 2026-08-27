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
	"strings"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

func testSpec() providers.MachineSpec {
	return providers.MachineSpec{
		Name:        "worker-1",
		VCPUs:       4,
		MemoryBytes: 8 << 30,
		DiskBytes:   40 << 30,
		Image:       providers.Image{Name: "ubuntu-24.04"},
		Networks:    []providers.Network{{Name: "br0"}},
	}
}

func TestDomainXMLIsWellFormedAndComplete(t *testing.T) {
	out := domainXML(testSpec(), "k8s-workers", "worker-1.qcow2")

	var parsed domainDef
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("generated domain XML does not parse: %v\n%s", err, out)
	}

	if parsed.Name != "worker-1" {
		t.Errorf("name = %q, want worker-1", parsed.Name)
	}
	if parsed.VCPU != 4 {
		t.Errorf("vcpu = %d, want 4", parsed.VCPU)
	}
	if parsed.Memory.Value != 8<<30 || parsed.Memory.Unit != unitBytes {
		t.Errorf("memory = %d %s, want %d bytes", parsed.Memory.Value, parsed.Memory.Unit, int64(8<<30))
	}
	if len(parsed.Devices.Interfaces) != 1 || parsed.Devices.Interfaces[0].Source.Bridge != "br0" {
		t.Errorf("expected a single interface bridged to br0, got %+v", parsed.Devices.Interfaces)
	}
	if len(parsed.Devices.Disks) != 1 || parsed.Devices.Disks[0].Source.Volume != "worker-1.qcow2" {
		t.Errorf("expected the disk to reference the created volume, got %+v", parsed.Devices.Disks)
	}

	// Without the guest agent channel, addresses can only come from DHCP leases,
	// which is a materially worse signal.
	if !strings.Contains(out, "org.qemu.guest_agent.0") {
		t.Error("domain XML is missing the QEMU guest agent channel")
	}
}

// A machine name is derived from a Kubernetes object name, so it cannot normally
// contain XML metacharacters -- but building this document by string
// concatenation would make that a one-character change away from a malformed or
// injected document. Marshalling makes it impossible.
func TestDomainXMLEscapesNames(t *testing.T) {
	spec := testSpec()
	spec.Name = `evil"><name>pwned</name><x a="`

	out := domainXML(spec, "pool", "vol")
	var parsed domainDef
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("generated XML does not parse: %v", err)
	}
	if parsed.Name != spec.Name {
		t.Errorf("name round-tripped as %q, want %q", parsed.Name, spec.Name)
	}
}

func TestVolumeXMLClonesFromBackingImage(t *testing.T) {
	out := volumeXML("worker-1.qcow2", "/var/lib/libvirt/images/base.qcow2", 40<<30)

	var parsed volumeDef
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("generated volume XML does not parse: %v\n%s", err, out)
	}
	if parsed.Capacity.Value != 40<<30 {
		t.Errorf("capacity = %d, want %d", parsed.Capacity.Value, int64(40<<30))
	}
	if parsed.Target.Format.Type != formatQcow2 {
		t.Errorf("format = %q, want %s", parsed.Target.Format.Type, formatQcow2)
	}
	// Copy-on-write from the base image rather than a full copy: a full copy would
	// make every machine creation pay the whole image size in time and disk.
	if parsed.BackingSt == nil || parsed.BackingSt.Path != "/var/lib/libvirt/images/base.qcow2" {
		t.Errorf("expected a backing store referencing the base image, got %+v", parsed.BackingSt)
	}
}

func TestUUIDRoundTrip(t *testing.T) {
	raw := golibvirt.UUID{
		0x4a, 0x8b, 0x1c, 0x2d, 0x3e, 0x4f, 0x50, 0x61,
		0x72, 0x83, 0x94, 0xa5, 0xb6, 0xc7, 0xd8, 0xe9,
	}
	s := formatUUID(raw)
	if want := "4a8b1c2d-3e4f-5061-7283-94a5b6c7d8e9"; s != want {
		t.Fatalf("formatUUID() = %q, want %q", s, want)
	}

	back, err := parseUUID(s)
	if err != nil {
		t.Fatalf("parseUUID(%q) returned error: %v", s, err)
	}
	if back != raw {
		t.Fatalf("parseUUID(formatUUID(x)) != x")
	}
}

func TestParseUUIDRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "not-a-uuid", "4a8b1c2d", strings.Repeat("z", 32)} {
		if _, err := parseUUID(in); err == nil {
			t.Errorf("parseUUID(%q) = nil error, want an error", in)
		}
	}
}
