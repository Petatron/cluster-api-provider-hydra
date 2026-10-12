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
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Each row of trackingDialer's abandonment table, in the order it lists them.

func TestTrackingDialerAbandonClosesARegisteredSocket(t *testing.T) {
	addr, accepted := recordingListener(t)
	d := newTrackingDialer(stubDialer{addr: addr}, time.Second)
	conn, err := d.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	peer := nextAccepted(t, accepted)

	c := d.abandonAttempt()
	if c != conn {
		t.Fatalf("abandonAttempt returned %v, want the registered connection", c)
	}
	_ = c.Close()
	waitClosedByPeer(t, peer)
	// Detached, so a later recycle cannot close it a second time.
	if got := d.abandonAttempt(); got != nil {
		t.Errorf("second abandonAttempt returned %v, want nil", got)
	}
}

// The inner dial may take the whole dial timeout to connect. Abandoning must end
// the attempt now, not when the connect finishes -- and the socket that finally
// arrives must still be closed, not leaked.
func TestTrackingDialerAbandonReleasesADialStillConnecting(t *testing.T) {
	addr, accepted := recordingListener(t)
	inner := newGatedDialer(t, addr)
	d := newTrackingDialer(inner, time.Minute)

	errs := make(chan error, 1)
	go func() {
		_, err := d.Dial()
		errs <- err
	}()
	inner.waitEntered(t)

	if c := d.abandonAttempt(); c != nil {
		t.Fatalf("abandonAttempt returned %v before any socket existed", c)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, errDialAbandoned) {
			t.Fatalf("Dial = %v, want errDialAbandoned", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dial was still waiting on the inner dial after its attempt was abandoned")
	}

	inner.release()
	waitClosedByPeer(t, nextAccepted(t, accepted))
}

// The remaining ordering: the inner dial completes in the same instant its
// attempt is abandoned, so Dial takes the result before it sees the abandonment.
// No hook can force that interleaving, so race the two many times and check the
// invariant each time: once both have returned, no socket is left registered,
// and a socket Dial handed out is the one the abandonment detached.
func TestTrackingDialerAbandonRacingRegistrationLeavesNoSocket(t *testing.T) {
	for range 2000 {
		d := newTrackingDialer(pipeDialer{}, time.Second)
		var (
			wg       sync.WaitGroup
			dialed   net.Conn
			dialErr  error
			detached net.Conn
		)
		start := make(chan struct{})
		wg.Go(func() { <-start; dialed, dialErr = d.Dial() })
		wg.Go(func() { <-start; detached = d.abandonAttempt() })
		close(start)
		wg.Wait()

		d.mu.Lock()
		leftover := d.conn
		d.mu.Unlock()
		if leftover != nil {
			t.Fatal("a socket was registered after its attempt was abandoned")
		}
		if dialErr == nil && dialed != detached {
			t.Fatal("Dial handed out a socket the abandonment did not detach")
		}
		for _, c := range []net.Conn{dialed, detached} {
			if c != nil {
				_ = c.Close()
			}
		}
	}
}

// pipeDialer connects instantly, to an in-memory pipe.
type pipeDialer struct{}

func (pipeDialer) Dial() (net.Conn, error) {
	c, s := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, s); _ = s.Close() }()
	return c, nil
}

func TestTrackingDialerAbandonedBeforeDialOpensNothing(t *testing.T) {
	inner := newGatedDialer(t, "127.0.0.1:1")
	inner.release()
	d := newTrackingDialer(inner, time.Second)

	_ = d.abandonAttempt()
	if _, err := d.Dial(); !errors.Is(err, errDialAbandoned) {
		t.Fatalf("Dial = %v, want errDialAbandoned", err)
	}
	// The inner dial would run in its own goroutine, so give it the chance.
	select {
	case <-inner.entered:
		t.Error("an abandoned attempt still started its inner dial")
	case <-time.After(200 * time.Millisecond):
	}
}

// The mark belongs to one attempt. If it outlived it, every reconnect after a
// single abandoned one would fail -- the provider could never connect again.
func TestTrackingDialerNextAttemptIsNotAbandoned(t *testing.T) {
	addr, accepted := recordingListener(t)
	inner := newGatedDialer(t, addr)
	d := newTrackingDialer(inner, 5*time.Second)
	_ = d.abandonAttempt()

	d.beginAttempt()
	errs := make(chan error, 1)
	go func() {
		_, err := d.Dial()
		errs <- err
	}()
	// Holding the inner dial open: a stale, already-closed abandonment channel
	// would release this Dial here instead of letting it connect.
	inner.waitEntered(t)
	time.Sleep(50 * time.Millisecond)
	inner.release()

	if err := <-errs; err != nil {
		t.Fatalf("Dial after beginAttempt = %v, want a connection", err)
	}
	peer := nextAccepted(t, accepted)
	d.forceClose()
	waitClosedByPeer(t, peer)
}

// forceClose is recycle's tool, for a live connection whose RPC stalled. It must
// not abandon anything: the reconnect that follows has to be allowed to dial.
func TestTrackingDialerForceCloseDoesNotAbandon(t *testing.T) {
	addr, _ := recordingListener(t)
	d := newTrackingDialer(stubDialer{addr: addr}, time.Second)
	d.forceClose()
	conn, err := d.Dial()
	if err != nil {
		t.Fatalf("Dial after forceClose = %v, want a connection", err)
	}
	_ = conn.Close()
}

func TestTrackingDialerAbandonToleratesNil(t *testing.T) {
	var d *trackingDialer
	d.beginAttempt()
	if c := d.abandonAttempt(); c != nil {
		t.Errorf("abandonAttempt on a nil dialer = %v, want nil", c)
	}
}

// recordingListener accepts connections, never writes to them, and hands the
// server side of each to the returned channel so a test can see whether the
// client closed it.
func recordingListener(t *testing.T) (string, <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted := make(chan net.Conn, 16)
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			accepted <- c
		}
	}()
	return ln.Addr().String(), accepted
}

func nextAccepted(t *testing.T, accepted <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-accepted:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection reached the listener")
		return nil
	}
}

// waitClosedByPeer fails unless the client end of peer is closed promptly.
// Whatever the client wrote before closing -- go-libvirt opens with its
// handshake -- is drained, not mistaken for an open connection.
func waitClosedByPeer(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, peer); isTimeout(err) {
		t.Fatalf("the client end is still open: %v", err)
	}
}

// assertStillOpen fails if the client end of peer is closed within a short wait.
func assertStillOpen(t *testing.T, peer net.Conn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := peer.Read(make([]byte, 1)); !isTimeout(err) {
		t.Fatalf("the client end was closed: read = %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// gatedDialer is an inner dialer that blocks inside Dial until released, the
// way a TCP connect to a slow peer does.
type gatedDialer struct {
	addr      string
	dials     atomic.Int32
	entered   chan struct{}
	enterOnce sync.Once
	gate      chan struct{}
	gateOnce  sync.Once
}

func newGatedDialer(t *testing.T, addr string) *gatedDialer {
	g := &gatedDialer{addr: addr, entered: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(g.release)
	return g
}

func (g *gatedDialer) Dial() (net.Conn, error) {
	g.dials.Add(1)
	g.enterOnce.Do(func() { close(g.entered) })
	<-g.gate
	return net.Dial("tcp", g.addr)
}

func (g *gatedDialer) release() { g.gateOnce.Do(func() { close(g.gate) }) }

func (g *gatedDialer) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the inner dial never started")
	}
}

// d.conn belongs to the current attempt. A new attempt detaches the previous
// one's socket, so abandoning the new attempt cannot reach back and return it.
func TestTrackingDialerBeginAttemptDetachesThePreviousSocket(t *testing.T) {
	addr, _ := recordingListener(t)
	d := newTrackingDialer(stubDialer{addr: addr}, time.Second)
	old, err := d.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if c := d.beginAttempt(); c != old {
		t.Fatalf("beginAttempt returned %v, want the previous attempt's socket", c)
	}
	_ = old.Close()
	if c := d.abandonAttempt(); c != nil {
		t.Errorf("abandoning the new attempt returned %v, the previous attempt's socket", c)
	}
}
