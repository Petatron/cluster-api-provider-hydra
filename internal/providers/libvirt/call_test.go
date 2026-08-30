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
	"net"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// testProvider is a Provider with no connection. recycle tolerates that, so the
// wrappers can be exercised without a hypervisor.
func testProvider() *Provider { return &Provider{} }

// The whole point of the call wrappers is that a stalled hypervisor cannot hold
// a reconcile worker forever. go-libvirt's generated API takes no context, so
// this behaviour lives entirely in these helpers and is worth testing directly.

func TestCallReturnsResultWhenFast(t *testing.T) {
	got, err := call(context.Background(), testProvider(), func() (int, error) { return 42, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("call() = %d, want 42", got)
	}
}

func TestCallPropagatesError(t *testing.T) {
	sentinel := errors.New("libvirt said no")
	if _, err := call(context.Background(), testProvider(), func() (int, error) { return 0, sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("call() error = %v, want %v", err, sentinel)
	}
}

func TestCallGivesUpWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	released := make(chan struct{})
	_, err := call(ctx, testProvider(), func() (int, error) {
		<-released // a hypervisor that never answers
		return 1, nil
	})
	close(released)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestCallVoidGivesUpWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	released := make(chan struct{})
	defer close(released)

	if err := callVoid(ctx, testProvider(), func() error { <-released; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("callVoid() error = %v, want context.Canceled", err)
	}
}

func TestCall2GivesUpWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	released := make(chan struct{})
	defer close(released)

	_, _, err := call2(ctx, testProvider(), func() (int32, int32, error) { <-released; return 0, 0, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("call2() error = %v, want context.Canceled", err)
	}
}

func TestCall2ReturnsBothValues(t *testing.T) {
	a, b, err := call2(context.Background(), testProvider(), func() (int32, int32, error) { return 7, 9, nil })
	if err != nil || a != 7 || b != 9 {
		t.Fatalf("call2() = (%d, %d, %v), want (7, 9, nil)", a, b, err)
	}
}

func TestWithRPCDeadlinePreservesExistingDeadline(t *testing.T) {
	p := &Provider{cfg: Config{RPCTimeout: time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	got, stop := p.withRPCDeadline(ctx)
	defer stop()

	gotDeadline, ok := got.Deadline()
	if !ok {
		t.Fatal("expected the existing deadline to be kept")
	}
	want, _ := ctx.Deadline()
	if !gotDeadline.Equal(want) {
		t.Fatalf("withRPCDeadline mutated a context that already had a deadline")
	}
}

func TestWithRPCDeadlineAddsOneWhenMissing(t *testing.T) {
	p := &Provider{cfg: Config{RPCTimeout: 50 * time.Millisecond}}
	got, stop := p.withRPCDeadline(context.Background())
	defer stop()
	if _, ok := got.Deadline(); !ok {
		t.Fatal("expected a deadline on a context that had none")
	}
}

// Error classification decides whether a machine is treated as absent, as
// already-present, or as a real failure. Getting these wrong makes teardown wedge
// or makes a retry loop forever, so each mapping is pinned.
func TestErrorClassification(t *testing.T) {
	mk := func(code golibvirt.ErrorNumber) error {
		return golibvirt.Error{Code: uint32(code), Message: "synthetic"}
	}

	for _, tc := range []struct {
		name  string
		err   error
		check func(error) bool
		want  bool
	}{
		{"no domain is not-found", mk(golibvirt.ErrNoDomain), isNotFound, true},
		{"no storage vol is not-found", mk(golibvirt.ErrNoStorageVol), isNotFound, true},
		{"no storage pool is not-found", mk(golibvirt.ErrNoStoragePool), isNotFound, true},
		{"invalid op is not not-found", mk(golibvirt.ErrOperationInvalid), isNotFound, false},
		{"plain error is not not-found", errors.New("boom"), isNotFound, false},

		{"vol exists is already-exists", mk(golibvirt.ErrStorageVolExist), isAlreadyExists, true},
		{"no domain is not already-exists", mk(golibvirt.ErrNoDomain), isAlreadyExists, false},

		{"invalid op is invalid-state", mk(golibvirt.ErrOperationInvalid), isInvalidState, true},
		{"no domain is not invalid-state", mk(golibvirt.ErrNoDomain), isInvalidState, false},
	} {
		if got := tc.check(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A timeout recycles the connection to release the parked RPC. With no
// connection there is nothing to drop, and it must not panic -- otherwise the
// first timeout during startup would take the manager down instead of retrying.
func TestRecycleToleratesNoConnection(t *testing.T) {
	testProvider().recycle()

	var nilProvider *Provider
	nilProvider.recycle()
}

// forceClose exists because go-libvirt's Disconnect negotiates: it sends a close
// RPC and waits for the reply. In the only case that matters -- a daemon that
// has stopped replying -- that blocks exactly as hard as the call it was meant
// to rescue. Closing the socket underneath does not negotiate.
func TestTrackingDialerForceClosesTheConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and never speak, which is the failure being modelled.
			defer func() { _ = c.Close() }()
		}
	}()

	d := newTrackingDialer(stubDialer{addr: ln.Addr().String()}, 5*time.Second)
	conn, err := d.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// A read would block indefinitely against this listener; forceClose must make
	// it return instead.
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := conn.Read(buf)
		readErr <- err
	}()

	d.forceClose()

	select {
	case <-readErr:
		// Released, which is the point.
	case <-time.After(5 * time.Second):
		t.Fatal("forceClose did not release a blocked read")
	}

	// Idempotent: teardown paths may call it more than once.
	d.forceClose()
}

func TestTrackingDialerForceCloseWithoutConnection(t *testing.T) {
	newTrackingDialer(stubDialer{addr: "127.0.0.1:1"}, time.Second).forceClose()
}

type stubDialer struct{ addr string }

func (s stubDialer) Dial() (net.Conn, error) { return net.Dial("tcp", s.addr) }
