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
// The bound is needed because the TLS dialer completes its handshake and then
// performs an unbounded read of libvirt's verification byte before returning the
// connection. Until it returns there is no net.Conn here, so forceClose has
// nothing to close and a peer that finishes TLS but never sends that byte would
// otherwise hang Dial forever.
//
// Known limitation: timing out unblocks the caller but cannot abort the dial
// itself, so that socket and goroutine stay alive until the peer acts or the OS
// gives up. On the TLS path specifically this leaks one of each per retry. A
// real fix needs a TLS dialer that applies a deadline to the verification read,
// which this dependency revision does not offer -- tracked as follow-up work
// rather than papered over here.
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
