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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt/socket/dialers"
)

const (
	testHost    = "libvirthost"
	testTCPPort = "16509"
	testTLSPort = "16514"
	testIPv4    = "10.0.0.8"
	testIPv6    = "2001:db8::a"
	testPKIPath = "/pki"
)

func TestSplitRemoteAddr(t *testing.T) {
	for _, tc := range []struct {
		in      string
		host    string
		port    string
		wantErr bool
	}{
		{in: testHost + ":" + testTCPPort, host: testHost, port: testTCPPort},
		{in: testHost, host: testHost},
		{in: testIPv4 + ":" + testTLSPort, host: testIPv4, port: testTLSPort},
		{in: testIPv4, host: testIPv4},
		{in: "[" + testIPv6 + "]:" + testTLSPort, host: testIPv6, port: testTLSPort},
		{in: testIPv6, host: testIPv6},
		{in: "[" + testIPv6 + "]", host: testIPv6},
		{in: "", wantErr: true},
		{in: testHost + ":" + testTCPPort + ":extra", wantErr: true},
	} {
		host, port, err := splitRemoteAddr(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitRemoteAddr(%q) = nil error, want an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitRemoteAddr(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if host != tc.host || port != tc.port {
			t.Errorf("splitRemoteAddr(%q) = (%q, %q), want (%q, %q)",
				tc.in, host, port, tc.host, tc.port)
		}
	}
}

func TestNewDialerSelectsTLSByDefault(t *testing.T) {
	d, err := newDialer(Config{
		RemoteAddr:  testHost + ":" + testTLSPort,
		PKIPath:     testPKIPath,
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("newDialer: %v", err)
	}
	tlsD, ok := d.(*tlsDialer)
	if !ok {
		t.Fatalf("newDialer() = %T, want *tlsDialer for a remote address", d)
	}
	// DialTimeout reaches the TLS dialer. go-libvirt's TLS dialer had no way to
	// take it, so the handshake used to run on its own 20s default.
	if tlsD.host != testHost || tlsD.port != testTLSPort || tlsD.timeout != 3*time.Second {
		t.Errorf("tlsDialer = %s:%s timeout %s, want %s:%s timeout 3s", tlsD.host, tlsD.port, tlsD.timeout, testHost, testTLSPort)
	}
	if tlsD.caDirs[0] != testPKIPath {
		t.Errorf("caDirs = %v, want the configured PKI path", tlsD.caDirs)
	}
}

func TestNewDialerTLSDefaultsThePort(t *testing.T) {
	d, err := newDialer(Config{RemoteAddr: testHost})
	if err != nil {
		t.Fatalf("newDialer: %v", err)
	}
	if got := d.(*tlsDialer).port; got != defaultTLSPort {
		t.Errorf("port = %q, want %q when the address names none", got, defaultTLSPort)
	}
}

func TestNewDialerInsecureUsesTCP(t *testing.T) {
	d, err := newDialer(Config{
		RemoteAddr:  testHost + ":" + testTCPPort,
		Insecure:    true,
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("newDialer: %v", err)
	}
	if _, ok := d.(*dialers.Remote); !ok {
		t.Fatalf("newDialer(Insecure) = %T, want *dialers.Remote", d)
	}
}

func TestNewDialerLocalUsesUnixSocket(t *testing.T) {
	d, err := newDialer(Config{DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("newDialer: %v", err)
	}
	if _, ok := d.(*dialers.Local); !ok {
		t.Fatalf("newDialer() = %T, want *dialers.Local when RemoteAddr is empty", d)
	}
}

func TestNewDialerRejectsMalformedAddr(t *testing.T) {
	_, err := newDialer(Config{RemoteAddr: testHost + ":" + testTCPPort + ":extra"})
	if err == nil {
		t.Fatal("newDialer accepted a malformed address")
	}
}

// Credentials are TLS material. Given for a connection that uses no TLS they
// would be silently ignored, so they are refused instead.
func TestNewDialerRefusesCredentialsWithoutTLS(t *testing.T) {
	creds := &TLSCredentials{CACert: []byte("x")}
	for name, cfg := range map[string]Config{
		"local socket": {Credentials: creds},
		"insecure TCP": {RemoteAddr: "hypervisor:16509", Insecure: true, Credentials: creds},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newDialer(cfg); err == nil || !strings.Contains(err.Error(), "does not use TLS") {
				t.Fatalf("newDialer = %v, want the credentials refused", err)
			}
		})
	}
}

func TestConfigHypervisorNamesTheEndpoint(t *testing.T) {
	const tls = "192.168.15.13:16514"
	for cfg, want := range map[Config]string{
		{}:                                      "local",
		{RemoteAddr: "192.168.15.13"}:           tls,
		{RemoteAddr: tls}:                       tls,
		{RemoteAddr: "hv", Insecure: true}:      "hv:16509",
		{RemoteAddr: "hv:2000", Insecure: true}: "hv:2000",
		{RemoteAddr: "[2001:db8::1]"}:           "[2001:db8::1]:16514",
		{RemoteAddr: "2001:db8::1"}:             "[2001:db8::1]:16514",
	} {
		if got := cfg.Hypervisor(); got != want {
			t.Errorf("Config{RemoteAddr: %q, Insecure: %v}.Hypervisor() = %q, want %q", cfg.RemoteAddr, cfg.Insecure, got, want)
		}
	}
}

// Credentials and a PKI path are two sources for one identity; with both, one
// would silently lose.
func TestNewDialerRefusesCredentialsWithAPKIPath(t *testing.T) {
	cfg := Config{RemoteAddr: "hypervisor:16514", PKIPath: "/etc/pki/libvirt", Credentials: &TLSCredentials{}}
	if _, err := newDialer(cfg); err == nil || !strings.Contains(err.Error(), "one or the other") {
		t.Fatalf("newDialer = %v, want the combination refused", err)
	}
}

// A failure building the in-memory dialer returns a nil interface, not a nil
// *tlsDialer wrapped in one.
func TestNewDialerReturnsANilDialerOnError(t *testing.T) {
	d, err := newDialer(Config{RemoteAddr: "hypervisor:16514", Credentials: &TLSCredentials{}})
	if err == nil {
		t.Fatal("newDialer accepted empty credentials")
	}
	if d != nil {
		t.Fatalf("newDialer returned %#v with its error, want a nil interface", d)
	}
}

// Credentials never print their key, however they are formatted or logged.
func TestTLSCredentialsDoNotPrintTheirKey(t *testing.T) {
	const secret = "-----BEGIN PRIVATE KEY-----sekrit"
	creds := TLSCredentials{CACert: []byte("ca"), ClientCert: []byte("cert"), ClientKey: []byte(secret)}
	// The key in every form a verb could render it: as text, and as the bytes
	// %d, %x, %X, %o and %b print a []byte as.
	byteVerbs := []string{"%d", "%x", "%X", "%o", "%b"}
	leaks := make([]string, 0, 1+len(byteVerbs))
	leaks = append(leaks, "sekrit")
	for _, verb := range byteVerbs {
		leaks = append(leaks, strings.Trim(fmt.Sprintf(verb, []byte("sekrit")), "[]"))
	}
	cfg := Config{Credentials: &creds}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%o", "%b"} {
		for _, v := range []any{creds, &creds, cfg, &cfg} {
			out := fmt.Sprintf(verb, v)
			for _, leak := range leaks {
				if strings.Contains(out, leak) {
					t.Errorf("%s of %T printed the key: %s", verb, v, out)
				}
			}
		}
	}
	if out := fmt.Sprint(creds.MarshalLog()); strings.Contains(out, "sekrit") {
		t.Errorf("MarshalLog printed the key: %s", out)
	}
}
