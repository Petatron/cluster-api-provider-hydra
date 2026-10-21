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
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt/socket"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// PET-67. go-libvirt's ConnectToURI replaces its disconnected channel only
// after the handshake succeeds:
//
//	err := l.socket.Connect()
//	go l.waitAndDisconnect()   // closes l.disconnected when the socket dies --
//	err = l.initLibvirtComms() //   unless it is already closed, as it is here
//	l.disconnected = make(chan struct{})
//
// A socket that dies between the last two statements is a disconnect go-libvirt
// never reports: waitAndDisconnect finds the old, closed channel and returns,
// ConnectToURI then installs an open one, and IsConnected is true from then on
// while every RPC fails with EINVAL on the dead socket. Its socket layer knows
// the socket is dead and would happily dial again; nothing asks it to.
//
// wedgeFixture reproduces that end state with the fake client, deterministically
// rather than by racing go-libvirt's goroutines:
//
//   - each connect dials the real socket through the provider's tracking dialer,
//     and a reader goroutine stands in for go-libvirt's listen loop on it;
//   - when that reader stops, RPCs fail with EINVAL, but the fake stays
//     "connected" -- the disconnect go-libvirt never reports;
//   - a connect while the previous reader still runs fails the way go-libvirt's
//     socket.Connect does, because its socket is still marked connected.
type wedgeFixture struct {
	t      *testing.T
	p      *Provider
	f      *fakeLibvirt
	daemon chan net.Conn // the daemon's side of each accepted connection

	mu         sync.Mutex
	reading    bool          // the listen loop for the current socket runs
	readerDone chan struct{} // closed when that loop exits
	sockets    int           // sockets dialled; a refused connect dials none

	// cleanup, when set, models go-libvirt noticing the disconnect after all:
	// that long after the reader stops, IsConnected goes false. A handshake
	// started inside that window has its callbacks dropped by the cleanup and
	// fails, as it would in go-libvirt's waitAndDisconnect.
	cleanup        time.Duration
	cleanupPending bool
}

func newWedgeFixture(t *testing.T) *wedgeFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	w := &wedgeFixture{t: t, daemon: make(chan net.Conn, 8)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
			w.daemon <- c
		}
	}()

	w.p, w.f = newFakeProvider(t)
	w.p.cfg.DialTimeout = 2 * time.Second
	w.p.cfg.RPCTimeout = 5 * time.Second
	w.p.dialer = newTrackingDialer(stubDialer{addr: ln.Addr().String()}, 5*time.Second)
	w.f.connected = false
	w.f.dial = w.connect
	return w
}

// connect is the socket half of go-libvirt's ConnectToURI.
func (w *wedgeFixture) connect() error {
	w.mu.Lock()
	if w.reading {
		w.mu.Unlock()
		return errors.New("already connected to socket") // go-libvirt's socket.Connect
	}
	if w.cleanupPending {
		w.mu.Unlock()
		return errors.New("handshake interrupted by the previous socket's cleanup")
	}
	w.mu.Unlock()

	conn, err := w.p.dialer.Dial()
	if err != nil {
		return err
	}
	done := make(chan struct{})
	w.mu.Lock()
	w.reading, w.readerDone = true, done
	w.sockets++
	w.mu.Unlock()
	w.f.mu.Lock()
	delete(w.f.errs, "DomainLookupByName")
	w.f.mu.Unlock()

	go func() {
		_, _ = io.Copy(io.Discard, conn)
		// The socket is dead: every RPC now fails as go-libvirt's SendPacket
		// does on a disconnected socket. IsConnected is left alone -- that is
		// the wedge.
		w.f.mu.Lock()
		if w.f.errs == nil {
			w.f.errs = map[string]error{}
		}
		w.f.errs["DomainLookupByName"] = syscall.EINVAL
		w.f.mu.Unlock()
		w.mu.Lock()
		w.reading = false
		cleanup := w.cleanup
		w.cleanupPending = cleanup > 0
		w.mu.Unlock()
		close(done)
		if cleanup > 0 {
			time.Sleep(cleanup)
			w.f.mu.Lock()
			w.f.connected = false
			w.f.mu.Unlock()
			w.mu.Lock()
			w.cleanupPending = false
			w.mu.Unlock()
		}
	}()
	return nil
}

// established makes the first connection and returns the daemon's end of it.
func (w *wedgeFixture) established() net.Conn {
	w.t.Helper()
	if _, err := w.p.FindByName(w.t.Context(), "worker-1"); !errors.Is(err, providers.ErrNotFound) {
		w.t.Fatalf("first FindByName = %v, want an ordinary lookup", err)
	}
	select {
	case c := <-w.daemon:
		return c
	case <-time.After(5 * time.Second):
		w.t.Fatal("the daemon never saw the first connection")
		return nil
	}
}

func (w *wedgeFixture) waitForReader(done chan struct{}) {
	w.t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		w.t.Fatal("the listen loop never saw its socket close")
	}
}

func (w *wedgeFixture) currentReader() chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.readerDone
}

// assertRecovers checks the next call redials and succeeds, rather than failing
// on the dead socket -- and keeps failing on every call after.
func (w *wedgeFixture) assertRecovers() {
	w.t.Helper()
	if _, err := w.p.FindByName(w.t.Context(), "worker-1"); !errors.Is(err, providers.ErrNotFound) {
		w.t.Fatalf("FindByName after the socket died = %v, want a redial and an ordinary lookup", err)
	}
	w.mu.Lock()
	n := w.sockets
	w.mu.Unlock()
	if n != 2 {
		w.t.Errorf("%d sockets dialled, want the first and one redial", n)
	}
}

// The daemon drops the connection. Only a socket-level signal can see this:
// the provider closed nothing, so nothing it did marks the socket dead.
func TestADaemonDropInsideTheHandshakeWindowDoesNotWedge(t *testing.T) {
	w := newWedgeFixture(t)
	daemonEnd := w.established()
	reader := w.currentReader()

	_ = daemonEnd.Close()
	w.waitForReader(reader)
	// The fake still reports connected, as go-libvirt does in the wedge;
	// TestRealGoLibvirtRecoversFromADropRightAfterOpen shows the real library
	// reaching that state.
	w.assertRecovers()
}

// recycle closes the socket after an RPC timeout.
func TestARecycledSocketDoesNotWedge(t *testing.T) {
	w := newWedgeFixture(t)
	w.established()
	reader := w.currentReader()

	w.p.recycle()
	w.waitForReader(reader)
	w.assertRecovers()
}

// An attempt abandoned in the moment after its handshake succeeded, while
// p.dial still names it (abandonDial's table): its socket is detached and
// closed, in the background, by whoever abandoned it. The next call must not
// trust that socket, even before the close has landed, and must get through
// even while go-libvirt's socket layer still thinks the old one is open.
func TestAnAttemptAbandonedAfterItsHandshakeDoesNotWedge(t *testing.T) {
	w := newWedgeFixture(t)
	w.established()

	detached := w.p.dialer.abandonAttempt()
	if detached == nil {
		t.Fatal("fixture: no socket to detach")
	}
	// The close lands only after the next call has started, so that call meets
	// a socket go-libvirt still considers connected.
	time.AfterFunc(200*time.Millisecond, func() { _ = detached.Close() })
	w.assertRecovers()
}

// A healthy connection is never redialled.
func TestALiveSocketIsNotRedialled(t *testing.T) {
	w := newWedgeFixture(t)
	w.established()
	for range 3 {
		if _, err := w.p.FindByName(t.Context(), "worker-1"); !errors.Is(err, providers.ErrNotFound) {
			t.Fatalf("FindByName = %v, want an ordinary lookup", err)
		}
	}
	if n := w.f.called("ConnectToURI"); n != 1 {
		t.Errorf("%d connects on a healthy socket, want 1", n)
	}
}

// connectOnce recognises go-libvirt's refusal by its text, so pin that text to
// the go-libvirt this module builds against: dial its real socket twice.
func TestSocketStillOpenMatchesGoLibvirt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	s := socket.New(stubDialer{addr: ln.Addr().String()}, discardRouter{})
	if err := s.Connect(); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Disconnect() })
	err = s.Connect()
	if err == nil || err.Error() != errSocketStillOpen {
		t.Fatalf("second Connect = %v, want go-libvirt's %q; connectOnce would stop retrying", err, errSocketStillOpen)
	}
}

type discardRouter struct{}

func (discardRouter) Route(*socket.Header, []byte) {}

// trackedConn calls a socket dead exactly when go-libvirt's listen loop gives up
// on it: a temporary read error leaves it alive, any other ends it.
func TestTrackedConnDiesWhenGoLibvirtsReaderWould(t *testing.T) {
	cases := []struct {
		name string
		err  error
		dead bool
	}{
		{"end of stream", io.EOF, true},
		{"permanent network error", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"temporary network error", &net.OpError{Op: "read", Err: temporaryErr{}}, false},
		{"no error", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &trackedConn{Conn: failingConn{err: tc.err}}
			_, _ = c.Read(make([]byte, 1))
			if c.isDead() != tc.dead {
				t.Errorf("dead = %v after a read returning %v, want %v", c.isDead(), tc.err, tc.dead)
			}
		})
	}

	c := &trackedConn{Conn: failingConn{}}
	_ = c.Close()
	if !c.isDead() {
		t.Error("a closed socket is not dead")
	}
}

type temporaryErr struct{}

func (temporaryErr) Error() string   { return "try again" }
func (temporaryErr) Timeout() bool   { return false }
func (temporaryErr) Temporary() bool { return true }

// failingConn is a net.Conn whose reads return err.
type failingConn struct {
	net.Conn
	err error
}

func (c failingConn) Read([]byte) (int, error) { return 0, c.err }
func (c failingConn) Close() error             { return nil }

// A refused redial does not outlive its attempt. go-libvirt refuses before it
// dials, so abandoning the attempt cannot end the refusal through Dial; the
// retry loop has to notice. Here the old socket never finishes closing, the
// caller gives up, and the attempt must end with it rather than hold every
// later call.
func TestARefusedRedialEndsWhenItsCallerGivesUp(t *testing.T) {
	w := newWedgeFixture(t)
	w.established()
	_ = w.p.dialer.abandonAttempt() // detached, and never closed

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := w.p.FindByName(ctx, "worker-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FindByName = %v, want the caller to give up on the refused redial", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		w.p.mu.Lock()
		inFlight := w.p.dial != nil
		w.p.mu.Unlock()
		if !inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refused redial kept its attempt claimed after its only caller gave up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// While a reconnect recovers from the wedge, IsConnected still says true and the
// new socket is already alive; only the in-flight attempt shows the handshake is
// unfinished. A caller arriving then must wait for it, not send RPCs on a
// connection that is not open yet.
func TestACallerDuringTheRecoveryWaitsForTheHandshake(t *testing.T) {
	w := newWedgeFixture(t)
	daemonEnd := w.established()
	reader := w.currentReader()
	_ = daemonEnd.Close()
	w.waitForReader(reader)

	// Hold the recovery's handshake after its socket exists.
	handshake := make(chan struct{})
	release := make(chan struct{})
	connect := w.connect
	w.f.dial = func() error {
		err := connect()
		close(handshake)
		<-release
		return err
	}

	first := make(chan error, 1)
	go func() {
		_, err := w.p.FindByName(t.Context(), "worker-1")
		first <- err
	}()
	<-handshake
	lookupsBefore := w.f.called("DomainLookupByName")

	second := make(chan error, 1)
	go func() {
		_, err := w.p.FindByName(t.Context(), "worker-1")
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("a caller finished while the handshake was held (err %v); it skipped the reconnect", err)
	case <-time.After(200 * time.Millisecond):
	}
	if n := w.f.called("DomainLookupByName"); n != lookupsBefore {
		t.Fatalf("%d lookups reached the hypervisor during the handshake, want none", n-lookupsBefore)
	}
	close(release)
	for _, ch := range []chan error{first, second} {
		if err := <-ch; !errors.Is(err, providers.ErrNotFound) {
			t.Errorf("FindByName = %v after the handshake, want an ordinary lookup", err)
		}
	}
}

// When go-libvirt does notice a disconnect, it does so a moment after the socket
// dies, and a handshake started inside that moment is torn down by its cleanup.
// socketLost sees the death first, so the reconnect has to wait out that moment
// -- as it effectively did before PET-67, when it began only once IsConnected
// went false -- rather than fail one call.
func TestTheReconnectWaitsForGoLibvirtsCleanup(t *testing.T) {
	w := newWedgeFixture(t)
	w.cleanup = 20 * time.Millisecond
	daemonEnd := w.established()
	reader := w.currentReader()

	_ = daemonEnd.Close()
	w.waitForReader(reader)
	w.assertRecovers()
}
