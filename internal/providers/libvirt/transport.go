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
	"net"
	"sync"

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
	inner socket.Dialer

	mu   sync.Mutex
	conn net.Conn
}

func newTrackingDialer(inner socket.Dialer) *trackingDialer {
	return &trackingDialer{inner: inner}
}

// Dial implements socket.Dialer.
func (d *trackingDialer) Dial() (net.Conn, error) {
	c, err := d.inner.Dial()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conn = c
	d.mu.Unlock()
	return c, nil
}

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
