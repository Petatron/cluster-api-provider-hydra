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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt/libvirttest"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

func TestBeginReconnectsADroppedConnection(t *testing.T) {
	p, f := newFakeProvider(t)
	f.connected = false

	_, err := p.FindByName(t.Context(), "worker-1")
	if !errors.Is(err, providers.ErrNotFound) || f.called("ConnectToURI") != 1 {
		t.Fatalf("FindByName = %v after %d dials; want one redial, then the lookup", err, f.called("ConnectToURI"))
	}
	if !f.IsConnected() {
		t.Error("the connection was not re-established")
	}
}

func TestBeginReportsAFailedReconnectWithoutCallingThrough(t *testing.T) {
	p, f := newFakeProvider(t)
	f.connected = false
	f.connectErr = errInjected

	_, err := p.FindByName(t.Context(), "worker-1")
	if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), "reconnecting") {
		t.Fatalf("FindByName = %v, want the reconnect failure", err)
	}
	if f.called("DomainLookupByName") != 0 {
		t.Error("an RPC was attempted on a connection that failed to come back")
	}
}

// A burst of reconciles against a slow hypervisor must produce one dial, not
// one per machine.
func TestConcurrentCallersShareOneReconnect(t *testing.T) {
	p, f := newFakeProvider(t)
	f.connected = false
	release := make(chan struct{})
	f.hook = func(method string) {
		if method == "ConnectToURI" {
			<-release
		}
	}

	const callers = 5
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			_, err := p.Get(t.Context(), "00000000-0000-0000-0000-0000000000ff")
			errs <- err
		})
	}
	// Hold the dial until every caller has found the connection down, so all of
	// them are either dialing or waiting on the dial.
	deadline := time.Now().Add(5 * time.Second)
	for f.connectedChecks() < callers {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d callers reached the connection check", f.connectedChecks(), callers)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if !errors.Is(err, providers.ErrNotFound) {
			t.Errorf("Get = %v, want machine not found after the shared reconnect", err)
		}
	}
	if n := f.called("ConnectToURI"); n != 1 {
		t.Errorf("%d dials for %d concurrent callers, want 1", n, callers)
	}
}

// A caller that gives up during a reconnect -- its context times out or is
// cancelled -- must not leave the dial claimed forever, or every later call
// would join an attempt that never ends.
func TestATimedOutReconnectDoesNotWedgeLaterCalls(t *testing.T) {
	p, f := newFakeProvider(t)
	p.cfg.RPCTimeout = time.Second
	p.dialer = newTrackingDialer(stubDialer{addr: silentListener(t)}, time.Second)
	f.connected = false
	var dials atomic.Int32
	// The first dial's completion channel, captured while that dial is certainly
	// in flight: ensureConnected sets p.dial before starting the dial and clears
	// it only after ConnectToURI returns. Reading p.dial after the timeout
	// instead could find it already cleared and skip the wait.
	stalled := make(chan chan struct{}, 1)
	// Closed once the first dial's socket is registered with the tracking
	// dialer. Waiting for it pins this test to the "after Dial registered the
	// socket" row of trackingDialer's table; the earlier rows have their own
	// tests (TestAnAbandonedReconnectEnds...).
	installed := make(chan struct{})
	f.dial = func() error {
		if dials.Add(1) > 1 {
			return nil
		}
		p.mu.Lock()
		stalled <- p.dial
		p.mu.Unlock()
		// The first dial waits on a peer that never answers. Only abandoning
		// the attempt, which closes its socket, can release it.
		conn, err := p.dialer.Dial()
		if err != nil {
			return err
		}
		close(installed)
		_, _ = io.ReadAll(conn)
		return errors.New("connection closed")
	}

	// The caller gives up by cancellation rather than by a timer, so it gives up
	// after the socket exists however late the dial goroutine runs.
	// ensureConnected handles both in the same ctx.Done() branch. The long
	// deadline only stops withRPCDeadline from adding a shorter one of its own.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		_, err := p.FindByName(ctx, "worker-1")
		errs <- err
	}()
	var abandoned chan struct{}
	select {
	case abandoned = <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the first dial never started")
	}
	select {
	case <-installed:
	case <-time.After(5 * time.Second):
		t.Fatal("the first dial never registered its connection")
	}
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "connecting") {
			t.Fatalf("FindByName = %v, want the reconnect abandoned", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FindByName did not return after its context was cancelled")
	}

	// The abandoning caller closes the socket and returns at once; the dial goroutine
	// still has to unwind and release its claim. A call made inside that window
	// joins the dying attempt and gets its error -- one failed reconcile, which
	// is requeued. What must never happen is the claim outliving the dial, so
	// wait for it to end, and fail if it does not.
	select {
	case <-abandoned:
	case <-time.After(5 * time.Second):
		t.Fatal("the timed-out dial never ended; every later call would join it")
	}

	if _, err := p.FindByName(t.Context(), "worker-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("FindByName after the stalled dial = %v, want an ordinary lookup", err)
	}
	if n := dials.Load(); n != 2 {
		t.Errorf("%d dials, want the stalled one and a fresh one", n)
	}
}

func TestCloseReturnsDisconnectsResult(t *testing.T) {
	p, f := newFakeProvider(t)
	if err := p.Close(); err != nil || f.IsConnected() {
		t.Fatalf("Close = %v, connected = %v; want a clean disconnect", err, f.IsConnected())
	}

	p, f = newFakeProvider(t)
	f.errs["Disconnect"] = errInjected
	if err := p.Close(); !errors.Is(err, errInjected) {
		t.Fatalf("Close = %v, want Disconnect's own result", err)
	}
}

// go-libvirt's Disconnect returns before closing its socket when the close RPC
// fails, so Close has to close it.
func TestCloseClosesTheSocketWhenDisconnectFails(t *testing.T) {
	p, f := newFakeProvider(t)
	p.dialer = newTrackingDialer(stubDialer{addr: silentListener(t)}, time.Second)
	conn, err := p.dialer.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	f.errs["Disconnect"] = errInjected

	if err := p.Close(); !errors.Is(err, errInjected) {
		t.Fatalf("Close = %v, want Disconnect's own result", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("read after Close = %v; want the socket closed", err)
	}
}

// silentListener accepts connections and never writes to them, the way a
// daemon that has stopped answering behaves. It returns the address.
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
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
		}
	}()
	return ln.Addr().String()
}

// New's handshake carries its own deadline: a daemon that accepts the socket
// and never answers would otherwise hang manager startup with no reconcile
// loop yet running to time it out. And the socket it gave up on must be closed:
// the dialer is discarded with the failed New, so nothing else ever would.
func TestNewGivesUpOnASilentDaemon(t *testing.T) {
	addr, accepted := recordingListener(t)

	start := time.Now()
	_, err := New(t.Context(), Config{
		RemoteAddr:  addr,
		Insecure:    true,
		DialTimeout: 100 * time.Millisecond,
		RPCTimeout:  100 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("New = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("New took %s against a silent daemon", elapsed)
	}
	waitClosedByPeer(t, nextAccepted(t, accepted))
}

func TestNewReportsAnUnreachableDaemon(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	_, err = New(t.Context(), Config{RemoteAddr: addr, Insecure: true, DialTimeout: time.Second})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("New = %v, want the dial failure itself", err)
	}
}

func TestNewRejectsAMalformedAddress(t *testing.T) {
	if _, err := New(t.Context(), Config{RemoteAddr: "a:b:c"}); err == nil {
		t.Fatal("New accepted a remote address that is not host or host:port")
	}
}

// The happy path, through the real go-libvirt client, against go-libvirt's own
// protocol mock behind a TCP listener.
func TestNewConnectsAndClosesAgainstALibvirtDaemon(t *testing.T) {
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
			daemon, _ := libvirttest.New().Dial()
			go func() { _, _ = io.Copy(daemon, c); _ = daemon.Close() }()
			go func() { _, _ = io.Copy(c, daemon); _ = c.Close() }()
		}
	}()

	p, err := New(t.Context(), Config{RemoteAddr: ln.Addr().String(), Insecure: true, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.cfg.RPCTimeout != defaultRPCTimeout {
		t.Errorf("RPCTimeout = %s, want the %s default", p.cfg.RPCTimeout, defaultRPCTimeout)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// PET-66: a caller that gives up before the dial has produced its socket. A
// plain forceClose finds nothing to close then; the socket that arrives later
// would sit in the handshake and hold the attempt open until some later caller
// timed out too.
func TestAnAbandonedReconnectEndsBeforeItsSocketExists(t *testing.T) {
	p, f := newFakeProvider(t)
	p.cfg.RPCTimeout = time.Second
	addr, accepted := recordingListener(t)
	p.dialer = newTrackingDialer(stubDialer{addr: addr}, time.Second)
	f.connected = false

	var dials atomic.Int32
	stalled := make(chan chan struct{}, 1)
	gate := make(chan struct{})
	firstDial := make(chan error, 1)
	f.dial = func() error {
		if dials.Add(1) > 1 {
			// Through the real dialer: the abandonment must not carry over and
			// fail the next attempt too.
			_, err := p.dialer.Dial()
			return err
		}
		p.mu.Lock()
		stalled <- p.dial
		p.mu.Unlock()
		<-gate // the dial goroutine runs late: its caller gives up first
		conn, err := p.dialer.Dial()
		firstDial <- err
		if err != nil {
			return err
		}
		_, _ = io.ReadAll(conn)
		return errors.New("connection closed")
	}

	// A real timer, which the late dial cannot race: it has not dialed at all.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := p.FindByName(ctx, "worker-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FindByName = %v, want the reconnect to time out", err)
	}
	abandoned := <-stalled
	close(gate)

	if err := <-firstDial; !errors.Is(err, errDialAbandoned) {
		t.Fatalf("the late dial = %v, want errDialAbandoned", err)
	}
	select {
	case <-abandoned:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned attempt never ended; every later call would join it")
	}
	select {
	case c := <-accepted:
		t.Fatalf("an abandoned attempt opened a socket (%s)", c.RemoteAddr())
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := p.FindByName(t.Context(), "worker-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("FindByName after the abandoned dial = %v, want an ordinary lookup", err)
	}
	if n := dials.Load(); n != 2 {
		t.Errorf("%d dials, want the abandoned one and a fresh one", n)
	}
}

// PET-66: a caller that gives up while the dial is still connecting. The
// attempt must end then -- not when the connect finally completes -- and the
// socket that completes must be closed.
func TestAnAbandonedReconnectEndsWhileItsDialIsConnecting(t *testing.T) {
	p, f := newFakeProvider(t)
	p.cfg.RPCTimeout = time.Second
	addr, accepted := recordingListener(t)
	inner := newGatedDialer(t, addr)
	p.dialer = newTrackingDialer(inner, time.Minute)
	f.connected = false

	var dials atomic.Int32
	stalled := make(chan chan struct{}, 1)
	f.dial = func() error {
		if dials.Add(1) > 1 {
			return nil
		}
		p.mu.Lock()
		stalled <- p.dial
		p.mu.Unlock()
		_, err := p.dialer.Dial()
		return err
	}

	// Give up only once the connect is under way, so this test stays on its row
	// however late the dial goroutine runs; giving up earlier is the "before
	// Dial" row, tested above. The long deadline only stops withRPCDeadline from
	// adding a shorter one of its own.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		_, err := p.FindByName(ctx, "worker-1")
		errs <- err
	}()
	inner.waitEntered(t)
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("FindByName = %v, want the reconnect abandoned", err)
	}
	// The connect is still held open by the gate.
	select {
	case <-<-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned attempt waited on a connect it no longer wanted")
	}

	inner.release()
	waitClosedByPeer(t, nextAccepted(t, accepted))

	if _, err := p.FindByName(t.Context(), "worker-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("FindByName after the abandoned dial = %v, want an ordinary lookup", err)
	}
}

// abandonDial's table, one row each. The provider is wired by hand so each row's
// state can be set exactly; a registered socket stands in for whatever
// connection exists at the moment the caller gives up.
func abandonDialFixture(t *testing.T) (*Provider, net.Conn) {
	t.Helper()
	p, _ := newFakeProvider(t)
	addr, accepted := recordingListener(t)
	p.dialer = newTrackingDialer(stubDialer{addr: addr}, time.Second)
	p.dialer.beginAttempt()
	if _, err := p.dialer.Dial(); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return p, nextAccepted(t, accepted)
}

func isAbandoned(d *trackingDialer) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.abandoned
}

func TestAbandonDialAbandonsTheAttemptInFlight(t *testing.T) {
	p, peer := abandonDialFixture(t)
	done := make(chan struct{})
	p.dial = done

	p.abandonDial(done)
	waitClosedByPeer(t, peer)
	if !isAbandoned(p.dialer) {
		t.Error("the attempt in flight was not marked abandoned")
	}
}

func TestAbandonDialAfterAFailedAttemptDoesNothing(t *testing.T) {
	p, _ := newFakeProvider(t)
	p.dialer = newTrackingDialer(stubDialer{addr: "127.0.0.1:1"}, time.Second)
	p.dialErr = errInjected // finished, failed: p.dial already cleared

	p.abandonDial(make(chan struct{}))
	if isAbandoned(p.dialer) {
		t.Error("an attempt that already failed was marked abandoned")
	}
}

// The caller's context ended in the same instant its attempt succeeded. The
// connection is healthy and other callers may already be using it.
func TestAbandonDialKeepsAConnectionThatSucceeded(t *testing.T) {
	p, peer := abandonDialFixture(t) // p.dial nil: the attempt finished

	p.abandonDial(make(chan struct{}))
	assertStillOpen(t, peer)
	if isAbandoned(p.dialer) {
		t.Error("a finished attempt was marked abandoned")
	}
}

// The caller's attempt finished and another caller started the next one before
// this caller's ctx.Done branch ran. That attempt is not this caller's to end.
func TestAbandonDialLeavesANewerAttemptAlone(t *testing.T) {
	p, peer := abandonDialFixture(t)
	p.dial = make(chan struct{}) // the newer attempt
	stale := make(chan struct{})

	p.abandonDial(stale)
	assertStillOpen(t, peer)
	if isAbandoned(p.dialer) {
		t.Error("the newer attempt was marked abandoned by a caller of the old one")
	}
}

// New's dialer is discarded when New fails, so a socket that arrived after New
// gave up would never be closed by anyone: a socket and a goroutine leaked per
// failed construction, and main retries construction on every reconcile.
func TestNewLeavesNoSocketBehindWhenItGivesUp(t *testing.T) {
	addr, accepted := recordingListener(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // gives up at once, before the dial goroutine can have dialed

	if _, err := New(ctx, Config{RemoteAddr: addr, Insecure: true, DialTimeout: time.Second}); !errors.Is(err, context.Canceled) {
		t.Fatalf("New = %v, want context.Canceled", err)
	}
	// Whichever way the race went, any socket that reached the listener must be
	// closed, and none may arrive later and stay open.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case c := <-accepted:
			waitClosedByPeer(t, c)
		case <-deadline:
			return
		}
	}
}
