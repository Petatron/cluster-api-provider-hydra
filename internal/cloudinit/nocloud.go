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

// Package cloudinit renders bootstrap data into something a virtual machine can
// actually consume at first boot.
//
// Cluster API's bootstrap providers hand over cloud-init user-data and stop
// there; delivering it is the infrastructure provider's problem. For a machine
// with no metadata service -- which is every libvirt guest -- the delivery
// mechanism is cloud-init's NoCloud datasource: a small ISO9660 filesystem
// labelled "cidata", attached as a CD-ROM, which cloud-init finds by label
// before the network exists.
//
// That last point is why this is not done over the network. Bootstrap data is
// what tells a machine how to reach the cluster, so anything requiring the
// cluster first would not terminate.
package cloudinit

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/kdomanski/iso9660"
	"sigs.k8s.io/yaml"
)

// VolumeLabel is the ISO9660 volume identifier cloud-init searches for. It is
// fixed by the NoCloud datasource, not chosen here: a differently labelled
// filesystem is simply never looked at, and the machine boots unconfigured.
const VolumeLabel = "cidata"

// maxUserDataBytes bounds what will be turned into an ISO.
//
// The whole image is assembled in memory on the controller, so an oversized
// Secret would otherwise be a way to exhaust the manager's memory from a
// namespace that can only create Machines. Real kubeadm join data is a few
// kilobytes; a megabyte is far beyond any legitimate use.
const maxUserDataBytes = 1 << 20

// ErrInvalid marks input that can never produce an image, however many times it
// is retried.
//
// The distinction is load-bearing for the caller, not cosmetic. A provider maps
// this to a terminal condition that a MachineHealthCheck may act on, so the
// errors that do NOT wrap it -- staging a temporary directory, writing the image
// -- must stay separable: a full disk is an operational failure that will pass,
// and reporting it as unrecoverable invites remediation of a machine that was
// about to be fine.
var ErrInvalid = errors.New("cloudinit: invalid input")

// ErrNoUserData reports that there is nothing to deliver. Callers should skip
// attaching a datasource entirely rather than producing an empty one, which
// cloud-init would read as an instruction to configure nothing.
var ErrNoUserData = fmt.Errorf("%w: no user data", ErrInvalid)

// Metadata is the small amount of instance identity NoCloud carries alongside
// the user-data.
type Metadata struct {
	// InstanceID is what cloud-init uses to decide whether it is looking at a new
	// machine. It must be stable for the life of the machine: cloud-init records
	// the last value it saw and re-runs per-instance modules -- including the
	// kubeadm join -- whenever it changes. A value that varied per boot would
	// re-join the cluster on every restart.
	InstanceID string

	// Hostname becomes the guest's hostname, and through it the name kubeadm
	// registers the Node under.
	//
	// Supplying it is not optional in practice. Machines are copy-on-write clones
	// of one base image, so without this every guest comes up with the hostname
	// baked into that image and they collide in the cluster the moment there is
	// more than one.
	Hostname string
}

// ISO renders bootstrap data into a NoCloud datasource image.
//
// The result is a complete ISO9660 filesystem, ready to be written to a volume
// and attached to a machine as a CD-ROM.
func ISO(meta Metadata, userData []byte) ([]byte, error) {
	if len(userData) == 0 {
		return nil, ErrNoUserData
	}
	if len(userData) > maxUserDataBytes {
		return nil, fmt.Errorf("%w: user data is %d bytes, which exceeds the %d byte limit",
			ErrInvalid, len(userData), maxUserDataBytes)
	}
	if meta.InstanceID == "" {
		return nil, fmt.Errorf("%w: instance id is required", ErrInvalid)
	}

	metaData, err := metadataYAML(meta)
	if err != nil {
		// Marshalling a map of strings, so this is unreachable in practice --
		// but it is a property of the input, not of the environment.
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	// NewWriter stages content in a temporary directory, so the user-data --
	// which contains a live cluster join token -- is briefly written to whatever
	// backs TMPDIR. The manager's Deployment mounts a memory-backed emptyDir there
	// for exactly this reason; see config/manager. Cleanup removes it either way.
	w, err := iso9660.NewWriter()
	if err != nil {
		return nil, fmt.Errorf("cloudinit: staging the datasource image: %w", err)
	}
	defer func() {
		// Best effort: a staging directory that outlives the call is a leak worth
		// logging but not worth failing an otherwise successful boot over. There is
		// no logger at this layer, so the caller's error is what surfaces.
		_ = w.Cleanup()
	}()

	// Both filenames are fixed by the NoCloud datasource. They are also longer
	// than plain ISO9660 permits and contain a hyphen, so the image needs the
	// Rock Ridge extensions this writer emits -- without them cloud-init finds
	// USER_DAT.;1 and concludes there is no datasource here.
	if err := w.AddFile(bytes.NewReader(userData), "user-data"); err != nil {
		return nil, fmt.Errorf("cloudinit: adding user-data: %w", err)
	}
	if err := w.AddFile(bytes.NewReader(metaData), "meta-data"); err != nil {
		return nil, fmt.Errorf("cloudinit: adding meta-data: %w", err)
	}

	var buf bytes.Buffer
	if err := w.WriteTo(&buf, VolumeLabel); err != nil {
		return nil, fmt.Errorf("cloudinit: writing the datasource image: %w", err)
	}
	return buf.Bytes(), nil
}

// metadataYAML renders the NoCloud meta-data document.
//
// Built through the YAML marshaller rather than by string concatenation, for the
// same reason the domain XML is: a hostname containing a colon or a newline
// should produce a quoted scalar, not a document that parses as something else.
func metadataYAML(meta Metadata) ([]byte, error) {
	doc := map[string]string{"instance-id": meta.InstanceID}
	if meta.Hostname != "" {
		// cloud-init accepts both spellings; local-hostname is the one the NoCloud
		// documentation uses and the one CABPK templates reference as
		// ds.meta_data.local_hostname.
		doc["local-hostname"] = meta.Hostname
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("cloudinit: rendering meta-data: %w", err)
	}
	return out, nil
}
