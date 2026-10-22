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

package providers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Petatron/cluster-api-provider-hydra/internal/metrics"
)

// fakeConns opens recording providers and counts opens and closes per key.
type fakeConns struct {
	mu     sync.Mutex
	opens  map[string]int
	closes map[string]int
	closed chan string
	fail   map[string]error
	gate   map[string]chan struct{} // an open for this key waits on it
	// closeErr is what closing returns. The pool logs it; a failed close must
	// still count as closed.
	closeErr error
}

func newFakeConns() *fakeConns {
	return &fakeConns{
		opens: map[string]int{}, closes: map[string]int{}, closed: make(chan string, 64),
		fail: map[string]error{}, gate: map[string]chan struct{}{},
	}
}

func (f *fakeConns) opener(key string) Opener {
	return func(context.Context) (MachineProvider, func() error, error) {
		f.mu.Lock()
		gate := f.gate[key]
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.opens[key]++
		if err := f.fail[key]; err != nil {
			return nil, nil, err
		}
		return &recordingProvider{}, func() error {
			f.mu.Lock()
			f.closes[key]++
			err := f.closeErr
			f.mu.Unlock()
			f.closed <- key
			return err
		}, nil
	}
}

func (f *fakeConns) count(m map[string]int, key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m[key]
}

// waitClosed waits for key's connection to be closed; the pool closes in the
// background.
func (f *fakeConns) waitClosed(t *testing.T, key string) {
	t.Helper()
	select {
	case got := <-f.closed:
		if got != key {
			t.Fatalf("closed %q, want %q", got, key)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("connection %q was never closed", key)
	}
}

// requireNoClose fails if any connection is closed within a short window.
func (f *fakeConns) requireNoClose(t *testing.T) {
	t.Helper()
	select {
	case got := <-f.closed:
		t.Fatalf("connection %q was closed while still in use", got)
	case <-time.After(100 * time.Millisecond):
	}
}

var (
	clusterA = types.NamespacedName{Namespace: "default", Name: "a"}
	clusterB = types.NamespacedName{Namespace: "default", Name: "b"}
)

func TestPoolSharesOneConnectionPerTarget(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	release := make(chan struct{})
	f.gate["hv1"] = release

	var wg sync.WaitGroup
	got := make([]MachineProvider, 10)
	for i := range got {
		wg.Go(func() {
			owner := clusterA
			if i%2 == 1 {
				owner = clusterB
			}
			p, err := pool.Get(t.Context(), owner, "hv1", "hv1:16514", f.opener("hv1"))
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			got[i] = p
		})
	}
	time.Sleep(50 * time.Millisecond) // let them all queue on the one open
	close(release)
	wg.Wait()

	if n := f.count(f.opens, "hv1"); n != 1 {
		t.Errorf("%d opens for one target, want 1", n)
	}
	for i := range got {
		if got[i] != got[0] {
			t.Fatalf("caller %d got a different provider", i)
		}
	}
}

func TestPoolDoesNotCacheAFailedOpen(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	f.fail["hv1"] = errors.New("unreachable")
	if _, err := pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1")); err == nil {
		t.Fatal("Get succeeded against a failing open")
	}
	if n := pool.entries(); n != 0 {
		t.Errorf("%d entries kept after a failed open, want none: a manager that kept them would grow with every unreachable target", n)
	}
	f.mu.Lock()
	delete(f.fail, "hv1")
	f.mu.Unlock()
	if _, err := pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1")); err != nil {
		t.Fatalf("Get after the hypervisor came back: %v", err)
	}
	if n := f.count(f.opens, "hv1"); n != 2 {
		t.Errorf("%d opens, want the failed one and a retry", n)
	}
}

// The last owner to leave closes the connection; any owner still using it keeps
// it open.
func TestPoolClosesAConnectionWhenItsLastOwnerLeaves(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	before := testutil.ToFloat64(metrics.ProviderConnections)

	for _, owner := range []types.NamespacedName{clusterA, clusterB} {
		if _, err := pool.Get(t.Context(), owner, "hv1", "hv1", f.opener("hv1")); err != nil {
			t.Fatal(err)
		}
	}
	if got := testutil.ToFloat64(metrics.ProviderConnections) - before; got != 1 {
		t.Errorf("connections gauge +%v, want +1", got)
	}

	pool.Release(t.Context(), clusterA)
	f.requireNoClose(t)
	pool.Release(t.Context(), clusterB)
	f.waitClosed(t, "hv1")
	if n := pool.entries(); n != 0 {
		t.Errorf("%d entries kept after the last owner left, want none", n)
	}
	if got := testutil.ToFloat64(metrics.ProviderConnections) - before; got != 0 {
		t.Errorf("connections gauge +%v after the last owner left, want back to where it was", got)
	}

	// A later Get opens afresh rather than handing out the closed connection.
	if _, err := pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1")); err != nil {
		t.Fatal(err)
	}
	if n := f.count(f.opens, "hv1"); n != 2 {
		t.Errorf("%d opens, want a fresh one after the close", n)
	}
}

// An owner that moves to another target -- credentials rotated, say -- releases
// the connection it used.
func TestPoolReleasesTheOldTargetWhenAnOwnerMoves(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	if _, err := pool.Get(t.Context(), clusterA, "hv1@v1", "hv1", f.opener("hv1@v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Get(t.Context(), clusterA, "hv1@v2", "hv1", f.opener("hv1@v2")); err != nil {
		t.Fatal(err)
	}
	f.waitClosed(t, "hv1@v1")
	if n := f.count(f.closes, "hv1@v2"); n != 0 {
		t.Errorf("the new connection was closed %d times", n)
	}
	// Asking again for the same target is not a move.
	if _, err := pool.Get(t.Context(), clusterA, "hv1@v2", "hv1", f.opener("hv1@v2")); err != nil {
		t.Fatal(err)
	}
	f.requireNoClose(t)
}

// The flag-configured connection, obtained with no owner, stays open however
// owners come and go.
func TestPoolNeverClosesAConnectionObtainedWithoutAnOwner(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	if _, err := pool.Get(t.Context(), types.NamespacedName{}, "flags", "local", f.opener("flags")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Get(t.Context(), clusterA, "flags", "local", f.opener("flags")); err != nil {
		t.Fatal(err)
	}
	pool.Release(t.Context(), clusterA)
	f.requireNoClose(t)
}

func TestPoolReleaseOfAnUnknownOwnerIsANoOp(t *testing.T) {
	var pool Pool
	pool.Release(t.Context(), clusterA)
	f := newFakeConns()
	if _, err := pool.Get(t.Context(), clusterB, "hv1", "hv1", f.opener("hv1")); err != nil {
		t.Fatal(err)
	}
	pool.Release(t.Context(), clusterA)
	f.requireNoClose(t)
}

// A connection's last owner leaving while another caller has reserved it, but
// not yet bound to it, must not close it from under that caller: it would be
// handed a closed connection.
func TestPoolDoesNotCloseAConnectionAnotherCallerHasReserved(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	if _, err := pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1")); err != nil {
		t.Fatal(err)
	}
	// clusterB reserves the connection; clusterA, its only owner, leaves before
	// clusterB has bound to it.
	pool.afterReserve = func() {
		pool.afterReserve = nil
		pool.Release(t.Context(), clusterA)
	}
	if _, err := pool.Get(t.Context(), clusterB, "hv1", "hv1", f.opener("hv1")); err != nil {
		t.Fatal(err)
	}
	f.requireNoClose(t)
	if n := f.count(f.opens, "hv1"); n != 1 {
		t.Errorf("%d opens, want clusterB to share the open connection", n)
	}
	pool.Release(t.Context(), clusterB)
	f.waitClosed(t, "hv1")
}

// entries is how many targets the pool tracks.
func (p *Pool) entries() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// Shutdown closes everything, the pinned connection included, and returns only
// once the closes have run.
func TestPoolCloseAllClosesEverythingAndWaits(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	before := testutil.ToFloat64(metrics.ProviderConnections)
	if _, err := pool.Get(t.Context(), types.NamespacedName{}, "flags", "local", f.opener("flags")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1")); err != nil {
		t.Fatal(err)
	}
	pool.CloseAll()
	if f.count(f.closes, "flags") != 1 || f.count(f.closes, "hv1") != 1 {
		t.Errorf("closes after CloseAll: flags %d, hv1 %d; want 1 each, done before it returned",
			f.count(f.closes, "flags"), f.count(f.closes, "hv1"))
	}
	if pool.entries() != 0 {
		t.Errorf("%d entries left after CloseAll", pool.entries())
	}
	if got := testutil.ToFloat64(metrics.ProviderConnections) - before; got != 0 {
		t.Errorf("connections gauge +%v after CloseAll, want back to where it was", got)
	}
}

// A close that reports an error still ends the connection's life in the pool.
func TestPoolCountsAFailedCloseAsClosed(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	f.closeErr = errors.New("close RPC failed")
	before := testutil.ToFloat64(metrics.ProviderConnections)
	if _, err := pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1")); err != nil {
		t.Fatal(err)
	}
	pool.Release(t.Context(), clusterA)
	f.waitClosed(t, "hv1")
	if got := testutil.ToFloat64(metrics.ProviderConnections) - before; got != 0 {
		t.Errorf("connections gauge +%v after a failed close, want back to where it was", got)
	}
	if pool.entries() != 0 {
		t.Errorf("%d entries kept after a failed close", pool.entries())
	}
}

func TestPoolRefusesWorkAfterCloseAll(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	pool.CloseAll()
	if _, err := pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1")); !errors.Is(err, errPoolClosed) {
		t.Fatalf("Get after CloseAll = %v, want errPoolClosed", err)
	}
	if n := f.count(f.opens, "hv1"); n != 0 {
		t.Errorf("%d opens after CloseAll, want none", n)
	}
}

// An open in flight when the manager shuts down finishes after CloseAll has
// run; its connection must be closed rather than left open with nothing to
// close it.
func TestPoolClosesAConnectionThatFinishesOpeningAfterCloseAll(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	before := testutil.ToFloat64(metrics.ProviderConnections)
	release := make(chan struct{})
	f.gate["hv1"] = release

	result := make(chan error, 1)
	go func() {
		_, err := pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1"))
		result <- err
	}()
	time.Sleep(50 * time.Millisecond)
	pool.CloseAll()
	close(release)

	if err := <-result; !errors.Is(err, errPoolClosed) {
		t.Fatalf("Get = %v, want errPoolClosed", err)
	}
	f.waitClosed(t, "hv1")
	if got := testutil.ToFloat64(metrics.ProviderConnections) - before; got != 0 {
		t.Errorf("connections gauge +%v, want back to where it was", got)
	}
}

// A caller queued behind someone else's dial gives up when its own context
// ends, not when that dial does.
func TestPoolCallerQueuedBehindADialHonoursItsOwnContext(t *testing.T) {
	var pool Pool
	f := newFakeConns()
	release := make(chan struct{})
	defer close(release)
	f.gate["hv1"] = release
	go func() { _, _ = pool.Get(t.Context(), clusterA, "hv1", "hv1", f.opener("hv1")) }()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := pool.Get(ctx, clusterB, "hv1", "hv1", f.opener("hv1")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Get = %v, want its own deadline", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("queued Get waited %s, want it to give up at its own 100ms deadline", waited)
	}
}
