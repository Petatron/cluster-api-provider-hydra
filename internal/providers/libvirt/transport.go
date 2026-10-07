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
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt/socket"
)

// trackingDialer remembers the connection it handed to go-libvirt so the
// transport can be force-closed later.
//
// This exists because go-libvirt's Disconnect is graceful: it sends a
// ProcConnectClose RPC and waits for the reply before closing the socket. In the
// one situation where dropping the connection actually matters -- a daemon that
// accepts the socket and then stops answering -- that reply never arrives, so
// Disconnect blocks exactly as hard as the call it was meant to rescue.
//
// Closing the net.Conn underneath it does not negotiate anything. Every parked
// read fails immediately, which is the whole point.
type trackingDialer struct {
	inner   socket.Dialer
	timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
}

func newTrackingDialer(inner socket.Dialer, timeout time.Duration) *trackingDialer {
	return &trackingDialer{inner: inner, timeout: timeout}
}

// Dial implements socket.Dialer, bounded so a stalled peer cannot block the
// caller indefinitely.
//
// Every inner dialer newDialer builds now bounds itself by DialTimeout: the
// local and plain-TCP dialers through their connect timeout, and tlsDialer with
// one deadline over its connect, handshake and verification read (PET-36).
// That deadline is what closes the socket of a peer that stalls mid-dial;
// before it, go-libvirt's TLS dialer read the verification byte with no
// deadline, and each retry against such a peer leaked a socket and a goroutine.
//
// So this timeout -- DialTimeout plus RPCTimeout, as New passes it -- is a
// backstop that a correct inner dialer never reaches. It is kept because the
// dialer is an interface: if one ever fails to bound itself, the caller is still
// released, and a connection that arrives late is closed rather than kept. What
// it cannot do is abort the inner dial, so a dialer that relies on it alone
// would hold a socket and a goroutine until the peer acts.
func (d *trackingDialer) Dial() (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := d.inner.Dial()
		ch <- result{c, err}
	}()

	timeout := d.timeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		d.mu.Lock()
		d.conn = r.conn
		d.mu.Unlock()
		return r.conn, nil
	case <-time.After(timeout):
		// Close the connection if it arrives late, so an abandoned dial does not
		// leave a socket open for the rest of the process lifetime.
		go func() {
			if r := <-ch; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("libvirt: dial did not complete within %s", timeout)
	}
}

// defaultDialTimeout bounds a dial when no timeout was configured.
const defaultDialTimeout = 30 * time.Second

// forceClose drops the current connection without negotiating.
//
// Safe to call when nothing is connected, and safe to call concurrently: the
// worst case is closing an already-closed connection, which returns an error
// that is deliberately ignored.
func (d *trackingDialer) forceClose() {
	d.mu.Lock()
	c := d.conn
	d.conn = nil
	d.mu.Unlock()

	if c != nil {
		_ = c.Close()
	}
}
