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
	"net"
	"strings"

	"github.com/digitalocean/go-libvirt/socket"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// newDialer builds the transport used to reach libvirt.
//
// Remote connections are TLS by default. go-libvirt's NewRemote is raw TCP
// with no server authentication, and the RPCs that travel over it can define,
// start and destroy domains -- privileged operations that must not cross an
// untrusted network in the clear. Plaintext TCP is available via Config.Insecure
// for a trusted tunnel (SSH, WireGuard) and nowhere else.
//
// NewRemote and NewTLS both treat their first argument as a hostname and append
// their own default port, so a documented "host:port" value such as
// hypervisor:16509 would otherwise be joined as [hypervisor:16509]:16509 and
// never connect. The address is split here and the port is passed with UsePort
// / UseTLSPort.
func newDialer(cfg Config) (socket.Dialer, error) {
	if cfg.RemoteAddr == "" {
		return dialers.NewLocal(dialers.WithLocalTimeout(cfg.DialTimeout)), nil
	}

	host, port, err := splitRemoteAddr(cfg.RemoteAddr)
	if err != nil {
		return nil, err
	}

	if cfg.Insecure {
		opts := []dialers.RemoteOption{dialers.WithRemoteTimeout(cfg.DialTimeout)}
		if port != "" {
			opts = append(opts, dialers.UsePort(port))
		}
		return dialers.NewRemote(host, opts...), nil
	}

	var opts []dialers.TLSOption
	if port != "" {
		opts = append(opts, dialers.UseTLSPort(port))
	}
	if cfg.PKIPath != "" {
		opts = append(opts, dialers.UsePKIPath(cfg.PKIPath))
	}
	return dialers.NewTLS(host, opts...), nil
}

// splitRemoteAddr accepts "host", "host:port", IPv6 literals, and "[IPv6]:port".
func splitRemoteAddr(addr string) (host, port string, err error) {
	if addr == "" {
		return "", "", fmt.Errorf("libvirt: remote address is empty")
	}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		return host, port, nil
	}
	// net.SplitHostPort requires a port. A bare host, a bare IPv6 literal, or
	// "[IPv6]" without a port are all valid: each transport has a default port.
	if len(addr) >= 2 && addr[0] == '[' && addr[len(addr)-1] == ']' {
		inner := addr[1 : len(addr)-1]
		if net.ParseIP(inner) != nil {
			return inner, "", nil
		}
	}
	if net.ParseIP(addr) != nil || !strings.Contains(addr, ":") {
		return addr, "", nil
	}
	return "", "", fmt.Errorf("libvirt: remote address %q is not host or host:port", addr)
}
