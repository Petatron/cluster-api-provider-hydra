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
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// The wedge tests in wedge_test.go model go-libvirt with the fake client, which
// is deterministic but only as faithful as the model. These drive the real,
// pinned go-libvirt against scriptDaemon: just enough of libvirtd's RPC
// protocol to authenticate, open a connection and answer a domain lookup, with
// the misbehaviour PET-67 is about scripted per connection.

// RPC procedure numbers scriptDaemon understands (libvirt's remote_protocol.x).
const (
	procConnectOpen        = 1
	procDomainLookupByName = 23
	procAuthList           = 66
)

// errNoDomain is VIR_ERR_NO_DOMAIN, which go-libvirt maps to providers.ErrNotFound
// here -- the ordinary result of looking up a domain that does not exist.
const errNoDomain = 42

type scriptDaemon struct {
	ln net.Listener

	mu sync.Mutex
	// scripts are the behaviours of the next connections, in order; once they
	// run out, a connection behaves normally.
	scripts []daemonScript
	// early counts RPCs, other than the handshake's, that arrived on a
	// connection before its ConnectOpen was answered.
	early int
}

type daemonScript int

const (
	// normalConn answers everything.
	normalConn daemonScript = iota
	// dropAfterOpen closes the connection right after answering ConnectOpen:
	// go-libvirt's handshake succeeds and the socket dies at once, inside the
	// window PET-67 is about.
	dropAfterOpen
	// slowOpen answers ConnectOpen half a second late, keeping a handshake in
	// flight.
	slowOpen
)

func startScriptDaemon(t *testing.T) *scriptDaemon {
	t.Helper()
	// These tests make go-libvirt drop a connection right after it opens, which
	// trips a data race inside go-libvirt itself: ConnectToURI writes
	// Libvirt.disconnected while the waitAndDisconnect goroutine it just started
	// reads it, without a lock. The provider cannot avoid that, and the race
	// detector would fail the tests for it. The provider's own synchronisation is
	// covered under -race by the fake-client tests in wedge_test.go. Upstream:
	// https://github.com/digitalocean/go-libvirt/issues/297
	if raceDetector {
		t.Skip("go-libvirt races on Libvirt.disconnected when a connection drops right after opening (digitalocean/go-libvirt#297)")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	d := &scriptDaemon{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(c, d.next())
		}
	}()
	return d
}

func (d *scriptDaemon) push(s ...daemonScript) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scripts = append(d.scripts, s...)
}

func (d *scriptDaemon) next() daemonScript {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.scripts) == 0 {
		return normalConn
	}
	s := d.scripts[0]
	d.scripts = d.scripts[1:]
	return s
}

func (d *scriptDaemon) earlyRPCs() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.early
}

func (d *scriptDaemon) provider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(t.Context(), Config{
		RemoteAddr: d.ln.Addr().String(), Insecure: true,
		DialTimeout: 2 * time.Second, RPCTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// serve answers one connection. Every packet is a 28-byte header -- length,
// program, version, procedure, type, serial, status -- and an XDR payload.
func (d *scriptDaemon) serve(c net.Conn, script daemonScript) {
	defer func() { _ = c.Close() }()
	var wmu sync.Mutex
	opened := false
	for {
		hdr := make([]byte, 28)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		body := make([]byte, int(binary.BigEndian.Uint32(hdr[0:4]))-28)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		proc := binary.BigEndian.Uint32(hdr[12:16])

		status, payload := uint32(0), binary.BigEndian.AppendUint32(nil, 0) // empty AuthList; void otherwise
		if proc == procDomainLookupByName {
			status, payload = 1, lookupError()
		}
		out := binary.BigEndian.AppendUint32(nil, uint32(28+len(payload)))
		out = append(out, hdr[4:16]...)                  // program, version, procedure
		out = binary.BigEndian.AppendUint32(out, 1)      // type: reply
		out = append(out, hdr[20:24]...)                 // serial
		out = binary.BigEndian.AppendUint32(out, status) // status
		out = append(out, payload...)

		wmu.Lock()
		if !opened && proc != procConnectOpen && proc != procAuthList {
			d.mu.Lock()
			d.early++
			d.mu.Unlock()
		}
		wmu.Unlock()

		if proc == procConnectOpen && script == slowOpen {
			go func() {
				time.Sleep(500 * time.Millisecond)
				wmu.Lock()
				opened = true
				_, _ = c.Write(out)
				wmu.Unlock()
			}()
			continue
		}
		wmu.Lock()
		if proc == procConnectOpen {
			opened = true
		}
		_, err := c.Write(out)
		wmu.Unlock()
		if err != nil || (proc == procConnectOpen && script == dropAfterOpen) {
			return
		}
	}
}

// lookupError is a remote_error saying the domain does not exist.
func lookupError() []byte {
	msg := "domain not found"
	b := binary.BigEndian.AppendUint32(nil, errNoDomain) // code
	b = binary.BigEndian.AppendUint32(b, 0)              // domain
	b = binary.BigEndian.AppendUint32(b, 1)              // message: present
	b = binary.BigEndian.AppendUint32(b, uint32(len(msg)))
	b = append(b, msg...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return binary.BigEndian.AppendUint32(b, 2) // level: error
}

func lookupOnce(p *Provider) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := p.FindByName(ctx, "worker-1")
	if errors.Is(err, providers.ErrNotFound) {
		return nil
	}
	if err == nil {
		return errors.New("found a domain the daemon does not have")
	}
	return err
}

// waitUntilLost waits for the dialer to see the dropped socket, so the next
// call is made after the drop rather than racing it.
func waitUntilLost(t *testing.T, p *Provider) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !p.dialer.socketLost(); {
		if time.Now().After(deadline) {
			t.Fatal("the dropped socket was never seen dead")
		}
		time.Sleep(time.Millisecond)
	}
}

// The daemon drops the connection the moment the handshake succeeds. Real
// go-libvirt usually misses that disconnect and reports itself connected for
// good; every call after must still get through.
func TestRealGoLibvirtRecoversFromADropRightAfterOpen(t *testing.T) {
	d := startScriptDaemon(t)
	const runs = 50
	wedged := 0
	for range runs {
		d.push(dropAfterOpen)
		p := d.provider(t)
		waitUntilLost(t, p)
		if p.lv.IsConnected() {
			wedged++
		}
		if err := lookupOnce(p); err != nil {
			t.Fatalf("the call after the drop = %v, want a redial and an ordinary lookup", err)
		}
	}
	// Not an assertion on the fix: evidence the test reaches the state it is
	// about. On a laptop go-libvirt wedges in about 199 of 200 runs.
	if wedged == 0 {
		t.Fatalf("go-libvirt reported the disconnect in all %d runs; the test never reached the wedge", runs)
	}
	t.Logf("go-libvirt wedged in %d of %d runs; every one recovered", wedged, runs)
}

// A caller that arrives while the recovery's handshake is still in flight waits
// for it, against real go-libvirt: no RPC reaches the daemon before ConnectOpen
// is answered.
func TestRealGoLibvirtCallerWaitsForTheRecoveryHandshake(t *testing.T) {
	d := startScriptDaemon(t)
	var p *Provider
	for range 50 {
		d.push(dropAfterOpen)
		p = d.provider(t)
		waitUntilLost(t, p)
		if p.lv.IsConnected() {
			break
		}
		p = nil
	}
	if p == nil {
		t.Skip("go-libvirt reported the disconnect in 50 runs; the wedge was not reached")
	}

	d.push(slowOpen)
	first := make(chan error, 1)
	go func() { first <- lookupOnce(p) }()
	for deadline := time.Now().Add(2 * time.Second); ; {
		p.mu.Lock()
		inFlight := p.dial != nil
		p.mu.Unlock()
		if inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the recovery never started")
		}
		time.Sleep(time.Millisecond)
	}
	if err := lookupOnce(p); err != nil {
		t.Errorf("the second caller = %v, want it to wait for the handshake and then look up", err)
	}
	if err := <-first; err != nil {
		t.Errorf("the first caller = %v, want an ordinary lookup", err)
	}
	if n := d.earlyRPCs(); n != 0 {
		t.Errorf("%d RPCs reached the daemon before ConnectOpen was answered, want none", n)
	}
}
