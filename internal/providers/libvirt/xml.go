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
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// The libvirt XML vocabulary, named rather than repeated as literals.
const (
	unitBytes     = "bytes"
	formatQcow2   = "qcow2"
	formatRaw     = "raw"
	modelVirtio   = "virtio"
	diskBusVirtio = modelVirtio
	diskBusSATA   = "sata"

	// diskTypeFile is the only disk type this provider emits. See domainXML for
	// why the pool/volume alternative is not an option.
	diskTypeFile = "file"
)

// libvirt's API is XML in and XML out. These types exist so the provider builds
// that XML through the marshaller rather than by string concatenation -- a
// machine name containing an angle bracket should produce an invalid name error
// from libvirt, not a malformed document that means something else entirely.

type domainDef struct {
	XMLName xml.Name  `xml:"domain"`
	Type    string    `xml:"type,attr"`
	Name    string    `xml:"name"`
	Memory  memoryDef `xml:"memory"`
	VCPU    int32     `xml:"vcpu"`
	OS      osDef     `xml:"os"`
	Devices devices   `xml:"devices"`
}

type memoryDef struct {
	Unit  string `xml:"unit,attr"`
	Value int64  `xml:",chardata"`
}

type osDef struct {
	Type osTypeDef `xml:"type"`
	Boot []bootDef `xml:"boot"`
}

type osTypeDef struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}

type bootDef struct {
	Dev string `xml:"dev,attr"`
}

type devices struct {
	Disks      []diskDef      `xml:"disk"`
	Interfaces []interfaceDef `xml:"interface"`
	Console    consoleDef     `xml:"console"`
	Channels   []channelDef   `xml:"channel"`
}

type diskDef struct {
	Type   string        `xml:"type,attr"`
	Device string        `xml:"device,attr"`
	Driver diskDriverDef `xml:"driver"`
	Source diskSourceDef `xml:"source"`
	Target diskTargetDef `xml:"target"`
	// ReadOnly renders as a bare <readonly/> element when set. A nil pointer is
	// omitted, which is what keeps it off the writable root disk.
	ReadOnly *readOnlyDef `xml:"readonly,omitempty"`
}

type readOnlyDef struct{}

type diskDriverDef struct {
	Name string `xml:"name,attr"`
	Type string `xml:"type,attr"`
}

// diskSourceDef carries only a file path. The pool/volume attributes libvirt
// also accepts are deliberately absent: see domainXML for why emitting them
// leaves the domain with no AppArmor rules for its own disks.
type diskSourceDef struct {
	File string `xml:"file,attr,omitempty"`
}

type diskTargetDef struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type interfaceDef struct {
	Type   string             `xml:"type,attr"`
	Source interfaceSourceDef `xml:"source"`
	Model  interfaceModelDef  `xml:"model"`
}

type interfaceSourceDef struct {
	Bridge string `xml:"bridge,attr"`
}

type interfaceModelDef struct {
	Type string `xml:"type,attr"`
}

type consoleDef struct {
	Type   string           `xml:"type,attr"`
	Target consoleTargetDef `xml:"target"`
}

type consoleTargetDef struct {
	Type string `xml:"type,attr"`
	Port string `xml:"port,attr"`
}

type channelDef struct {
	Type   string           `xml:"type,attr"`
	Target channelTargetDef `xml:"target"`
}

type channelTargetDef struct {
	Type string `xml:"type,attr"`
	Name string `xml:"name,attr"`
}

type volumeDef struct {
	XMLName    xml.Name        `xml:"volume"`
	Name       string          `xml:"name"`
	Capacity   volCapacityDef  `xml:"capacity"`
	Target     volTargetDef    `xml:"target"`
	BackingSt  *volBackingDef  `xml:"backingStore,omitempty"`
	Allocation *volCapacityDef `xml:"allocation,omitempty"`
}

type volCapacityDef struct {
	Unit  string `xml:"unit,attr"`
	Value int64  `xml:",chardata"`
}

type volTargetDef struct {
	Format volFormatDef `xml:"format"`
}

type volFormatDef struct {
	Type string `xml:"type,attr"`
}

type volBackingDef struct {
	Path   string       `xml:"path"`
	Format volFormatDef `xml:"format"`
}

// domainXML renders the libvirt domain definition for a machine.
//
// Disks are referenced by absolute host path -- <disk type='file'> with a
// <source file=> -- and NOT by <source pool= volume=>, even though the volumes
// were just created through the pool API and the pool form would be the more
// natural expression of that.
//
// The reason is AppArmor, which is enabled by default on Ubuntu and most other
// distributions that ship libvirt. libvirt generates a per-domain AppArmor
// profile by handing the domain XML to virt-aa-helper, which walks the disks and
// emits a rule for each path it finds -- including, for a copy-on-write clone,
// the backing file underneath it. virt-aa-helper does not resolve the
// pool/volume form: it sees a disk with no path and emits nothing at all. The
// domain then starts with a profile that grants it no disk access, and qemu
// fails with "Could not open ... Permission denied" on the first image it
// touches. The file mode is irrelevant -- a world-readable backing image is
// denied just the same, which is what makes the failure so misleading.
//
// So the paths are resolved by the caller and spelled out here. Both forms are
// equivalent to libvirt itself; only the security driver can tell them apart.
//
// cidataPath is the cloud-init NoCloud image, attached as a CD-ROM. An empty
// value attaches nothing, which is the no-bootstrap-data case: the machine boots
// the base image and configures nothing.
func domainXML(spec providers.MachineSpec, rootPath, cidataPath string) string {
	d := domainDef{
		Type:   "kvm",
		Name:   spec.Name,
		Memory: memoryDef{Unit: unitBytes, Value: spec.MemoryBytes},
		VCPU:   spec.VCPUs,
		OS: osDef{
			Type: osTypeDef{Arch: "x86_64", Machine: "q35", Value: "hvm"},
			Boot: []bootDef{{Dev: "hd"}},
		},
		Devices: devices{
			Disks: []diskDef{{
				Type:   diskTypeFile,
				Device: "disk",
				Driver: diskDriverDef{Name: "qemu", Type: formatQcow2},
				Source: diskSourceDef{File: rootPath},
				Target: diskTargetDef{Dev: "vda", Bus: diskBusVirtio},
			}},
			Console: consoleDef{
				Type:   "pty",
				Target: consoleTargetDef{Type: "serial", Port: "0"},
			},
			// The guest agent channel is what lets DomainInterfaceAddresses report
			// addresses from inside the guest rather than only from DHCP leases.
			Channels: []channelDef{{
				Type:   "unix",
				Target: channelTargetDef{Type: "virtio", Name: "org.qemu.guest_agent.0"},
			}},
		},
	}
	if cidataPath != "" {
		// SATA rather than virtio, and sda rather than vda. cloud-init's NoCloud
		// datasource finds this by filesystem label, so the bus does not strictly
		// matter -- but a virtio CD-ROM is not something every guest image's
		// initramfs has drivers for at the point cloud-init runs, and a SATA
		// optical device is the configuration base cloud images are built and
		// tested against.
		d.Devices.Disks = append(d.Devices.Disks, diskDef{
			Type:     diskTypeFile,
			Device:   "cdrom",
			Driver:   diskDriverDef{Name: "qemu", Type: formatRaw},
			Source:   diskSourceDef{File: cidataPath},
			Target:   diskTargetDef{Dev: "sda", Bus: diskBusSATA},
			ReadOnly: &readOnlyDef{},
		})
	}

	for _, n := range spec.Networks {
		d.Devices.Interfaces = append(d.Devices.Interfaces, interfaceDef{
			Type:   "bridge",
			Source: interfaceSourceDef{Bridge: n.Name},
			Model:  interfaceModelDef{Type: modelVirtio},
		})
	}

	out, err := xml.Marshal(d)
	if err != nil {
		// Marshalling a struct of strings and ints cannot fail; if it somehow does,
		// an empty document makes libvirt reject it loudly rather than silently
		// defining something unintended.
		return ""
	}
	return string(out)
}

// volumeXML renders a qcow2 volume backed by the base image, so machines are
// copy-on-write clones rather than full copies.
func volumeXML(name, baseImage string, sizeBytes int64) string {
	v := volumeDef{
		Name:     name,
		Capacity: volCapacityDef{Unit: unitBytes, Value: sizeBytes},
		Target:   volTargetDef{Format: volFormatDef{Type: formatQcow2}},
		BackingSt: &volBackingDef{
			Path:   baseImage,
			Format: volFormatDef{Type: formatQcow2},
		},
	}
	out, err := xml.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

// rawVolumeXML renders a plain raw volume with no backing store, used for the
// cloud-init image.
//
// Unlike a machine's root disk this is not a copy-on-write clone of anything --
// it is a few hundred kilobytes of ISO9660 uploaded verbatim, and giving it a
// backing store would make libvirt read the base image underneath it.
func rawVolumeXML(name string, sizeBytes int64) string {
	v := volumeDef{
		Name:     name,
		Capacity: volCapacityDef{Unit: unitBytes, Value: sizeBytes},
		Target:   volTargetDef{Format: volFormatDef{Type: formatRaw}},
	}
	out, err := xml.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

// formatUUID renders libvirt's raw 16-byte UUID as the canonical hyphenated
// string, which is what ends up inside the providerID.
func formatUUID(u golibvirt.UUID) string {
	h := hex.EncodeToString(u[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// parseUUID is formatUUID's inverse, used to turn the ID stored in a providerID
// back into something libvirt will accept.
func parseUUID(s string) (golibvirt.UUID, error) {
	var u golibvirt.UUID
	stripped := strings.ReplaceAll(s, "-", "")
	if len(stripped) != 32 {
		return u, fmt.Errorf("machine id %q is not a UUID", s)
	}
	raw, err := hex.DecodeString(stripped)
	if err != nil {
		return u, fmt.Errorf("machine id %q is not a UUID: %w", s, err)
	}
	copy(u[:], raw)
	return u, nil
}
