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

package cloudinit

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kdomanski/iso9660"
	"sigs.k8s.io/yaml"
)

// readISO reads the produced image back as a filesystem.
//
// Reading it back matters more here than in most tests. Nothing in this
// provider has yet booted a real machine, so a malformed image would not be
// caught anywhere else until a VM silently came up unconfigured -- the failure
// this package exists to prevent.
func readISO(t *testing.T, data []byte) map[string]string {
	t.Helper()

	img, err := iso9660.OpenImage(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("produced image is not a readable ISO9660 filesystem: %v", err)
	}

	label, err := img.Label()
	if err != nil {
		t.Fatalf("reading volume label: %v", err)
	}
	// cloud-init finds this datasource by label and by nothing else. A wrong
	// label is not a degraded image, it is an invisible one.
	if !strings.EqualFold(strings.TrimSpace(label), VolumeLabel) {
		t.Errorf("volume label = %q, want %q", label, VolumeLabel)
	}

	root, err := img.RootDir()
	if err != nil {
		t.Fatalf("reading root directory: %v", err)
	}
	children, err := root.GetChildren()
	if err != nil {
		t.Fatalf("listing root directory: %v", err)
	}

	out := make(map[string]string, len(children))
	for _, c := range children {
		body, err := io.ReadAll(c.Reader())
		if err != nil {
			t.Fatalf("reading %q: %v", c.Name(), err)
		}
		out[c.Name()] = string(body)
	}
	return out
}

const testHostname = "worker-1"

func TestISOCarriesUserDataAndMetadata(t *testing.T) {
	userData := []byte("#cloud-config\nruncmd:\n  - kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef\n")

	iso, err := ISO(Metadata{InstanceID: "ns-worker-1-abc123", Hostname: testHostname}, userData)
	if err != nil {
		t.Fatalf("ISO() returned error: %v", err)
	}

	files := readISO(t, iso)

	// The names are fixed by the NoCloud datasource. Plain ISO9660 would have
	// mangled these to USER_DAT.;1, which cloud-init does not look for -- so this
	// assertion is really checking that the Rock Ridge extensions are present.
	got, ok := files["user-data"]
	if !ok {
		t.Fatalf("image has no user-data file; contains %v", keysOf(files))
	}
	if got != string(userData) {
		t.Errorf("user-data round-tripped as %q, want %q", got, userData)
	}

	rawMeta, ok := files["meta-data"]
	if !ok {
		t.Fatalf("image has no meta-data file; contains %v", keysOf(files))
	}
	var meta map[string]string
	if err := yaml.Unmarshal([]byte(rawMeta), &meta); err != nil {
		t.Fatalf("meta-data is not valid YAML: %v\n%s", err, rawMeta)
	}
	if meta["instance-id"] != "ns-worker-1-abc123" {
		t.Errorf("instance-id = %q, want ns-worker-1-abc123", meta["instance-id"])
	}
	// Without this every machine cloned from one base image answers to the same
	// hostname, and they collide as Nodes the moment there is more than one.
	if meta["local-hostname"] != testHostname {
		t.Errorf("local-hostname = %q, want %q", meta["local-hostname"], testHostname)
	}
}

// A hostname is not attacker-controlled in practice, but building the document
// by concatenation would put it one character away from meaning something else.
func TestISOEscapesMetadataValues(t *testing.T) {
	hostname := "evil\ninstance-id: hijacked"

	iso, err := ISO(Metadata{InstanceID: "real-id", Hostname: hostname}, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("ISO() returned error: %v", err)
	}

	var meta map[string]string
	if err := yaml.Unmarshal([]byte(readISO(t, iso)["meta-data"]), &meta); err != nil {
		t.Fatalf("meta-data is not valid YAML: %v", err)
	}
	if meta["instance-id"] != "real-id" {
		t.Errorf("instance-id = %q; a hostname overwrote it", meta["instance-id"])
	}
	if meta["local-hostname"] != hostname {
		t.Errorf("local-hostname = %q, want it preserved verbatim", meta["local-hostname"])
	}
}

func TestISOOmitsHostnameWhenUnset(t *testing.T) {
	iso, err := ISO(Metadata{InstanceID: "id-only"}, []byte("#cloud-config\n"))
	if err != nil {
		t.Fatalf("ISO() returned error: %v", err)
	}

	var meta map[string]string
	if err := yaml.Unmarshal([]byte(readISO(t, iso)["meta-data"]), &meta); err != nil {
		t.Fatalf("meta-data is not valid YAML: %v", err)
	}
	// An empty local-hostname would set the guest's hostname to the empty string,
	// which is worse than leaving the image's own.
	if _, present := meta["local-hostname"]; present {
		t.Errorf("local-hostname is present with no hostname configured: %v", meta)
	}
}

func TestISORejectsEmptyUserData(t *testing.T) {
	// Attaching an empty datasource is not the same as attaching none: cloud-init
	// would find it and conclude it has been told to configure nothing, rather
	// than looking for a datasource elsewhere.
	if _, err := ISO(Metadata{InstanceID: "x"}, nil); !errors.Is(err, ErrNoUserData) {
		t.Errorf("ISO(nil) error = %v, want ErrNoUserData", err)
	}
	if _, err := ISO(Metadata{InstanceID: "x"}, []byte{}); !errors.Is(err, ErrNoUserData) {
		t.Errorf("ISO(empty) error = %v, want ErrNoUserData", err)
	}
}

func TestISORejectsOversizedUserData(t *testing.T) {
	// The image is assembled in the controller's memory, so this bound is what
	// stops an oversized Secret from being a way to exhaust the manager.
	_, err := ISO(Metadata{InstanceID: "x"}, bytes.Repeat([]byte("a"), maxUserDataBytes+1))
	if err == nil {
		t.Fatal("ISO() accepted user data past the size limit")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to name the size limit", err)
	}
}

func TestISORequiresInstanceID(t *testing.T) {
	// cloud-init keys "have I already configured this machine?" on the instance
	// id. Without one it re-runs per-instance modules on every boot, which for a
	// Kubernetes node means re-running kubeadm join.
	if _, err := ISO(Metadata{Hostname: testHostname}, []byte("#cloud-config\n")); err == nil {
		t.Error("ISO() accepted metadata with no instance id")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
