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
	out := domainXML(testSpec(), "k8s-workers", "worker-1.qcow2", "")

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

	out := domainXML(spec, "pool", "vol", "")
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

// The guest agent reports every interface it can see. Publishing loopback or
// link-local as InternalIP would be worse than reporting nothing: the controller
// treats any IP as addressed and drops to a slow health interval, so a loopback
// arriving first would freeze status at 127.0.0.1.
func TestIsRoutable(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"192.168.15.42", true},
		{"10.0.0.5", true},
		{"2001:db8::1", true},

		{"127.0.0.1", false},
		{"224.0.0.1", false},       // multicast identifies a group, not this guest
		{"255.255.255.255", false}, // broadcast
		{"ff02::1", false},         // IPv6 all-nodes multicast
		{"::1", false},
		{"169.254.10.5", false},
		{"fe80::1", false},
		{"0.0.0.0", false},
		{"", false},
		{"not-an-ip", false},
	} {
		if got := isRoutable(tc.addr); got != tc.want {
			t.Errorf("isRoutable(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// Deletion must find the disk the domain actually uses, not the one current
// config would point at. A redeploy with a different --libvirt-storage-pool is
// enough to diverge, and looking in the wrong pool would report the volume
// absent, undefine the domain, and strand the real disk with nothing pointing
// at it.
func TestDomainXMLCarriesItsDiskSource(t *testing.T) {
	out := domainXML(testSpec(), "pool-the-machine-was-built-in", "worker-1.qcow2", "")

	var parsed domainDef
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("generated domain XML does not parse: %v", err)
	}

	var pool, volume string
	for _, d := range parsed.Devices.Disks {
		if d.Source.Volume != "" {
			pool, volume = d.Source.Pool, d.Source.Volume
			break
		}
	}
	if pool != "pool-the-machine-was-built-in" {
		t.Errorf("disk pool = %q, want the pool the domain was defined with", pool)
	}
	if volume != "worker-1.qcow2" {
		t.Errorf("disk volume = %q, want worker-1.qcow2", volume)
	}
}

// A machine with bootstrap data gets a second disk. Everything about deletion
// depends on that disk being visible in the domain XML, because that is where
// teardown looks for what to reclaim.
func TestDomainXMLAttachesCloudInitImage(t *testing.T) {
	out := domainXML(testSpec(), "k8s-workers", "worker-1.qcow2", "worker-1-cidata.iso")

	var parsed domainDef
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("generated domain XML does not parse: %v\n%s", err, out)
	}

	if len(parsed.Devices.Disks) != 2 {
		t.Fatalf("expected a root disk and a cloud-init CD-ROM, got %d disks", len(parsed.Devices.Disks))
	}

	var cdrom *diskDef
	for i := range parsed.Devices.Disks {
		if parsed.Devices.Disks[i].Device == "cdrom" {
			cdrom = &parsed.Devices.Disks[i]
		}
	}
	if cdrom == nil {
		t.Fatalf("no CD-ROM device in %+v", parsed.Devices.Disks)
	}
	if cdrom.Source.Volume != "worker-1-cidata.iso" {
		t.Errorf("cdrom volume = %q, want worker-1-cidata.iso", cdrom.Source.Volume)
	}
	if cdrom.Source.Pool != "k8s-workers" {
		t.Errorf("cdrom pool = %q, want k8s-workers", cdrom.Source.Pool)
	}
	// An ISO read as qcow2 is not a filesystem cloud-init can mount.
	if cdrom.Driver.Type != formatRaw {
		t.Errorf("cdrom driver type = %q, want %s", cdrom.Driver.Type, formatRaw)
	}
	// A writable datasource would let a guest rewrite its own bootstrap data,
	// and a CD-ROM that is not read-only is not what cloud images expect.
	if cdrom.ReadOnly == nil {
		t.Error("cloud-init CD-ROM is not marked read-only")
	}
}

// The no-bootstrap-data case has to stay clean: an empty cidata volume name must
// attach no device at all rather than one pointing at "".
func TestDomainXMLOmitsCloudInitWhenAbsent(t *testing.T) {
	out := domainXML(testSpec(), "pool", "vol", "")

	var parsed domainDef
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("generated domain XML does not parse: %v", err)
	}
	if len(parsed.Devices.Disks) != 1 {
		t.Fatalf("expected only the root disk, got %+v", parsed.Devices.Disks)
	}
	if strings.Contains(out, "cdrom") {
		t.Error("domain XML mentions a cdrom with no bootstrap data")
	}
	// The root disk must never inherit the CD-ROM's read-only flag.
	if parsed.Devices.Disks[0].ReadOnly != nil {
		t.Error("root disk is marked read-only")
	}
}

func TestRawVolumeXMLHasNoBackingStore(t *testing.T) {
	out := rawVolumeXML("worker-1-cidata.iso", 366592)

	var parsed volumeDef
	if err := xml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("generated volume XML does not parse: %v\n%s", err, out)
	}
	if parsed.Capacity.Value != 366592 {
		t.Errorf("capacity = %d, want 366592", parsed.Capacity.Value)
	}
	if parsed.Target.Format.Type != formatRaw {
		t.Errorf("format = %q, want %s", parsed.Target.Format.Type, formatRaw)
	}
	// A backing store here would make libvirt read the base image underneath the
	// uploaded ISO.
	if parsed.BackingSt != nil {
		t.Errorf("cloud-init volume has a backing store: %+v", parsed.BackingSt)
	}
}
