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

package providers

import "testing"

// The providerID format is load-bearing: the Kubernetes Node reports the same
// string, and Cluster API pairs Machine to Node by comparing them. A change here
// silently breaks that association rather than failing loudly, so it is worth
// pinning.
func TestProviderIDRoundTrip(t *testing.T) {
	const (
		backend = "libvirt"
		id      = "4a8b1c2d-3e4f-5061-7283-94a5b6c7d8e9"
	)

	got := ProviderID(backend, id)
	if want := "hydra://libvirt/4a8b1c2d-3e4f-5061-7283-94a5b6c7d8e9"; got != want {
		t.Fatalf("ProviderID() = %q, want %q", got, want)
	}

	gotBackend, gotID, err := ParseProviderID(got)
	if err != nil {
		t.Fatalf("ParseProviderID(%q) returned error: %v", got, err)
	}
	if gotBackend != backend || gotID != id {
		t.Fatalf("ParseProviderID() = (%q, %q), want (%q, %q)", gotBackend, gotID, backend, id)
	}
}

func TestParseProviderIDRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"hydra://",
		"hydra://libvirt",     // no id
		"hydra://libvirt/",    // empty id
		"hydra:///abc",        // empty backend
		"aws:///i-0123456789", // another provider's ID
		"libvirt/4a8b1c2d",    // no scheme
	} {
		if _, _, err := ParseProviderID(in); err == nil {
			t.Errorf("ParseProviderID(%q) = nil error, want an error", in)
		}
	}
}
