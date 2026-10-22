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
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitalocean/go-libvirt/socket"
)

// errDialAbandoned is returned by a dial whose attempt was abandoned: every
// caller waiting on it gave up, or its time limit ran out.
var errDialAbandoned = errors.New("libvirt: connection attempt abandoned")

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
//
// Closing only works on a socket that exists, though. An attempt can be given
// up on, by its callers or by its time limit, before its dial has produced one
// -- the dial has not started, or is still connecting -- and the socket that
// arrives afterwards would then sit in the libvirt handshake against a daemon
// that never answers, holding the attempt open (PET-66). So giving up is recorded against the
// attempt, not just applied to whatever socket happens to be registered:
//
//	when the attempt is abandoned          | what happens to its socket
//	---------------------------------------+------------------------------------
//	after Dial registered the socket       | closed now
//	while the inner dial is still running  | Dial fails now; socket closed on arrival
//	before Dial was called                 | never opened; Dial fails at once
//
// The mark lasts until the next attempt begins, and attempts never overlap:
// the next one starts only after ConnectToURI -- and so this Dial -- returned.
type trackingDialer struct {
	inner   socket.Dialer
	timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
	// last is the socket most recently handed to go-libvirt. Unlike conn it is
	// not cleared when the socket is detached: it is what socketLost asks about
	// (PET-67).
	last *trackedConn
	// abandoned is set when the current attempt is abandoned, and cleared when
	// the next attempt begins. abandonedCh is closed at the same
	// moment, to release a Dial waiting on the inner dial.
	abandoned   bool
	abandonedCh chan struct{}
}

func newTrackingDialer(inner socket.Dialer, timeout time.Duration) *trackingDialer {
	return &trackingDialer{inner: inner, timeout: timeout, abandonedCh: make(chan struct{})}
}

// beginAttempt starts a new connection attempt, clearing any abandonment of the
// previous one, and detaches the previous attempt's socket so that d.conn only
// ever belongs to the current attempt. The caller closes the returned
// connection, outside any lock it holds, as with abandonAttempt. Tolerates a
// nil dialer, as recycle does.
func (d *trackingDialer) beginAttempt() net.Conn {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.abandoned {
		d.abandoned = false
		d.abandonedCh = make(chan struct{})
	}
	return d.detachLocked()
}

// detachLocked detaches the current socket for the caller to close, and marks
// it dead now rather than when that close lands: the close runs in the
// background, and until it does nothing else would tell socketLost. d.mu must
// be held.
func (d *trackingDialer) detachLocked() net.Conn {
	c := d.conn
	d.conn = nil
	if tc, ok := c.(*trackedConn); ok {
		tc.markDead()
	}
	return c
}

// socketLost reports whether the socket go-libvirt was last given is dead: its
// reader has seen the error go-libvirt's own listen loop stops on, or it was
// closed or detached. go-libvirt does not always report that itself (PET-67), so
// ensureConnected asks this as well as IsConnected.
//
// A dialer that has handed out no socket has nothing to report, and neither
// does a nil one; both say false, leaving IsConnected the only signal.
func (d *trackingDialer) socketLost() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	last := d.last
	d.mu.Unlock()
	return last != nil && last.isDead()
}

// abandonAttempt records that the current attempt has been given up on, and
// detaches its socket if it has one. The caller closes the returned connection,
// outside any lock it holds: closing a TLS connection writes an alert and can
// block on a peer that has stopped reading.
func (d *trackingDialer) abandonAttempt() net.Conn {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.abandoned {
		d.abandoned = true
		close(d.abandonedCh)
	}
	return d.detachLocked()
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
	d.mu.Lock()
	abandoned, abandonedCh := d.abandoned, d.abandonedCh
	d.mu.Unlock()
	if abandoned {
		return nil, errDialAbandoned
	}

	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := d.inner.Dial()
		ch <- result{c, err}
	}()
	// Close the connection if it arrives late, so an abandoned dial does not
	// leave a socket open for the rest of the process lifetime.
	closeOnArrival := func() {
		go func() {
			if r := <-ch; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
	}

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
		if d.abandoned {
			d.mu.Unlock()
			// In the background, as everywhere a socket is abandoned: until
			// Dial returns, ConnectToURI cannot, and the abandoned attempt
			// stays claimed.
			closeDetached(r.conn)
			return nil, errDialAbandoned
		}
		tc := &trackedConn{Conn: r.conn}
		d.conn, d.last = tc, tc
		d.mu.Unlock()
		return tc, nil
	case <-abandonedCh:
		closeOnArrival()
		return nil, errDialAbandoned
	case <-time.After(timeout):
		closeOnArrival()
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
	c := d.detachLocked()
	d.mu.Unlock()

	if c != nil {
		_ = c.Close()
	}
}

// trackedConn is the socket handed to go-libvirt, recording when it dies the way
// go-libvirt's own listen loop decides it has: a read error that is not
// temporary, or a close (PET-67). go-libvirt's socket layer knows the same thing
// but does not expose it, and its Libvirt can miss it entirely.
type trackedConn struct {
	net.Conn
	dead atomic.Bool
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil && !temporaryReadError(err) {
		c.dead.Store(true)
	}
	return n, err
}

func (c *trackedConn) Close() error {
	c.dead.Store(true)
	return c.Conn.Close()
}

func (c *trackedConn) markDead()    { c.dead.Store(true) }
func (c *trackedConn) isDead() bool { return c.dead.Load() }

// temporaryReadError mirrors go-libvirt's socket.isTemporary, the test its listen
// loop applies to decide whether a read error ends the connection, so trackedConn
// calls a socket dead whenever go-libvirt's reader would stop -- and possibly a
// little earlier: that loop skips a failed header or payload read and stops only
// at the next length read, which fails the same way on a closed socket, and
// detaching marks a socket dead before its close lands. Earlier is safe: a
// reconnect started then dials on a fresh client (see ensureConnected).
//
// A plain type assertion, not errors.As, because that is what go-libvirt uses.
func temporaryReadError(err error) bool {
	opErr, ok := err.(*net.OpError) //nolint:errorlint // must match go-libvirt's assertion exactly
	if ok {
		return opErr.Temporary() //nolint:staticcheck // the deprecated check go-libvirt itself applies
	}
	return false
}
