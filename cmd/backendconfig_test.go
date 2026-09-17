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

package main

import (
	"strings"
	"testing"

	libvirtprovider "github.com/Petatron/cluster-api-provider-hydra/internal/providers/libvirt"
)

func TestEnvOr(t *testing.T) {
	t.Run("unset falls back to the default", func(t *testing.T) {
		if got := envOr("HYDRA_TEST_UNSET_VAR", "fallback"); got != "fallback" {
			t.Fatalf("envOr = %q, want %q", got, "fallback")
		}
	})

	t.Run("set wins over the default", func(t *testing.T) {
		t.Setenv("HYDRA_TEST_VAR", "from-env")
		if got := envOr("HYDRA_TEST_VAR", "fallback"); got != "from-env" {
			t.Fatalf("envOr = %q, want %q", got, "from-env")
		}
	})

	// An empty ConfigMap value is the shape the old shipped placeholders had --
	// remoteAddr: "" and storagePool: "". Treating empty as "unset" is what keeps
	// an explicitly-blank key from being different to a missing one.
	t.Run("empty is treated as unset", func(t *testing.T) {
		t.Setenv("HYDRA_TEST_VAR", "")
		if got := envOr("HYDRA_TEST_VAR", "fallback"); got != "fallback" {
			t.Fatalf("envOr = %q, want %q", got, "fallback")
		}
	})
}

func TestEnvBoolOr(t *testing.T) {
	t.Run("unset falls back", func(t *testing.T) {
		got, err := envBoolOr("HYDRA_TEST_UNSET_BOOL", true)
		if err != nil || !got {
			t.Fatalf("envBoolOr = (%v, %v), want (true, nil)", got, err)
		}
	})

	for _, v := range []string{"true", "TRUE", "1", "t"} {
		t.Run("parses "+v, func(t *testing.T) {
			t.Setenv("HYDRA_TEST_BOOL", v)
			got, err := envBoolOr("HYDRA_TEST_BOOL", false)
			if err != nil || !got {
				t.Fatalf("envBoolOr(%q) = (%v, %v), want (true, nil)", v, got, err)
			}
		})
	}

	// The safe direction matters more than the error here: this boolean decides
	// TLS versus plaintext for privileged domain RPCs, so a value nobody can
	// parse must not be read as "insecure = true".
	t.Run("garbage reports an error and defaults to the safe value", func(t *testing.T) {
		t.Setenv("HYDRA_TEST_BOOL", "yes-please")
		got, err := envBoolOr("HYDRA_TEST_BOOL", false)
		if err == nil {
			t.Fatal("envBoolOr returned nil error for an unparseable value")
		}
		if got {
			t.Fatal("envBoolOr defaulted to true (insecure) for an unparseable value")
		}
		if !strings.Contains(err.Error(), "HYDRA_TEST_BOOL") {
			t.Fatalf("error %q does not name the variable", err)
		}
	})
}

// A remote address with the TLS port, used wherever the test only needs "some
// remote address" and the transport is what is under test.
const testTLSAddr = "h:16514"

func TestDescribeBackend(t *testing.T) {
	kv := func(pairs []any) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[pairs[i].(string)] = pairs[i+1].(string)
		}
		return m
	}

	t.Run("local socket when no remote address", func(t *testing.T) {
		got := kv(describeBackend(libvirtprovider.Config{URI: "qemu:///system"}))
		if !strings.Contains(got["transport"], defaultLocalSocket) {
			t.Fatalf("transport = %q, want it to name %q", got["transport"], defaultLocalSocket)
		}
	})

	// The distinction that matters operationally: the same address reads very
	// differently depending on whether it is encrypted.
	t.Run("remote distinguishes TLS from plaintext", func(t *testing.T) {
		tlsCfg := kv(describeBackend(libvirtprovider.Config{RemoteAddr: testTLSAddr}))
		if !strings.Contains(tlsCfg["transport"], "TLS") {
			t.Fatalf("transport = %q, want TLS", tlsCfg["transport"])
		}
		plain := kv(describeBackend(libvirtprovider.Config{RemoteAddr: "h:16509", Insecure: true}))
		if !strings.Contains(plain["transport"], "plaintext") {
			t.Fatalf("transport = %q, want plaintext", plain["transport"])
		}
	})

	t.Run("unset optional values are visible as unset", func(t *testing.T) {
		got := kv(describeBackend(libvirtprovider.Config{RemoteAddr: testTLSAddr}))
		if got["storagePool"] != unsetPlaceholder || got["baseImage"] != unsetPlaceholder {
			t.Fatalf("storagePool=%q baseImage=%q, want %q for both",
				got["storagePool"], got["baseImage"], unsetPlaceholder)
		}
	})
}

func TestBackendWarning(t *testing.T) {
	absent := func(string) bool { return false }
	present := func(string) bool { return true }

	t.Run("remote address configured is never warned about", func(t *testing.T) {
		if err := backendWarning(libvirtprovider.Config{RemoteAddr: testTLSAddr}, true, absent); err != nil {
			t.Fatalf("warning = %v, want none", err)
		}
	})

	t.Run("local socket present is fine", func(t *testing.T) {
		if err := backendWarning(libvirtprovider.Config{}, false, present); err != nil {
			t.Fatalf("warning = %v, want none", err)
		}
	})

	// This is the PET-45 failure exactly: in a pod, no remote address, no socket
	// mounted. It used to be visible only once a machine reconcile failed.
	//
	// The names are asserted rather than just the word "ConfigMap": the most
	// likely route into this state is the rename, where `kubectl get cm` shows
	// the legacy object looking perfectly healthy. A message that does not say
	// which name the manager actually reads sends the operator the wrong way.
	t.Run("in-cluster names both the expected and the legacy ConfigMap", func(t *testing.T) {
		err := backendWarning(libvirtprovider.Config{}, true, absent)
		if err == nil {
			t.Fatal("no warning for the in-cluster misconfiguration")
		}
		msg := err.Error()
		for _, want := range []string{
			defaultLocalSocket,
			backendConfigMapName,
			legacyBackendConfigMapName,
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("warning %q does not name %q", msg, want)
			}
		}
	})

	t.Run("out of cluster still warns but does not mention the ConfigMap", func(t *testing.T) {
		err := backendWarning(libvirtprovider.Config{}, false, absent)
		if err == nil {
			t.Fatal("no warning when the socket is genuinely missing")
		}
		if strings.Contains(err.Error(), "ConfigMap") {
			t.Fatalf("warning %q mentions the ConfigMap outside the cluster", err)
		}
	})
}
