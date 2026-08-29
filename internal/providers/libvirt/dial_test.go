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
	d, err := newDialer(Config{RemoteAddr: testHost + ":" + testTLSPort})
	if err != nil {
		t.Fatalf("newDialer: %v", err)
	}
	if _, ok := d.(*dialers.TLS); !ok {
		t.Fatalf("newDialer() = %T, want *dialers.TLS for a remote address", d)
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
