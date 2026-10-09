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
	time.Sleep(50 * time.Millisecond)
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

// A caller that times out during a reconnect must not leave the dial claimed
// forever, or every later call would join an attempt that never ends.
func TestATimedOutReconnectDoesNotWedgeLaterCalls(t *testing.T) {
	p, f := newFakeProvider(t)
	p.dialer = newTrackingDialer(stubDialer{addr: "127.0.0.1:1"}, time.Second)
	f.connected = false
	release := make(chan struct{})
	var once sync.Once
	f.hook = func(method string) {
		if method == "ConnectToURI" {
			once.Do(func() { <-release })
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := p.FindByName(ctx, "worker-1")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "connecting") {
		t.Fatalf("FindByName = %v, want the reconnect to time out", err)
	}

	close(release)
	if _, err := p.FindByName(t.Context(), "worker-1"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("FindByName after the stalled dial = %v, want an ordinary lookup", err)
	}
}

func TestCloseDisconnectsGracefully(t *testing.T) {
	p, f := newFakeProvider(t)
	f.errs["Disconnect"] = errInjected

	if err := p.Close(); !errors.Is(err, errInjected) {
		t.Fatalf("Close = %v, want Disconnect's own result", err)
	}
	if f.IsConnected() {
		t.Error("still connected after Close")
	}
}

// New's handshake carries its own deadline: a daemon that accepts the socket
// and never answers would otherwise hang manager startup with no reconcile
// loop yet running to time it out.
func TestNewGivesUpOnASilentDaemon(t *testing.T) {
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

	start := time.Now()
	_, err = New(t.Context(), Config{
		RemoteAddr:  ln.Addr().String(),
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
