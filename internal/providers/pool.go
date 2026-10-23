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

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Petatron/cluster-api-provider-hydra/internal/metrics"
)

// Pool holds one backend connection per target and closes a connection once no
// cluster uses it any more (PET-38).
//
// A target is whatever identifies one connection -- for libvirt, a hypervisor
// address together with the credentials used to reach it. The pool knows only
// its key: the caller decides what goes into it, so the pool stays backend-free
// like the rest of this package.
//
// Owners are what make closing possible. A cluster that resolves its provider
// becomes an owner of that target's connection; when it moves to another
// target, or is gone, it releases the old one, and the last owner to leave
// closes it. A connection obtained with GetPinned is never closed: that is the
// manager-wide connection configured by flags, which clusters naming no
// hypervisor share, exactly as before the pool existed. Pinning is explicit
// rather than implied by a missing owner, so a caller that fails to produce an
// owner gets an error instead of a connection nothing will ever close.
//
// The zero value is ready to use.
type Pool struct {
	mu     sync.Mutex
	conns  map[string]*poolEntry
	owners map[types.NamespacedName]string
	// closed is set by CloseAll; the pool refuses all work afterwards.
	closed bool

	// afterReserve, when set, runs in Get between reserving an entry and
	// binding to it -- the window a concurrent Release must not close the entry
	// in. Tests only.
	afterReserve func()
}

// poolEntry is one target's connection.
type poolEntry struct {
	name string

	// open serialises opening, so concurrent callers for one target share one
	// dial instead of racing several. A channel rather than a mutex, so a
	// caller queued behind someone else's dial can give up when its own
	// context ends instead of waiting out that dial's whole timeout.
	open chan struct{}

	// Guarded by Pool.mu.
	prov    MachineProvider
	release func() error
	owners  map[types.NamespacedName]struct{}
	pinned  bool // obtained with GetPinned: never closed
	pending int  // callers between reserving this entry and binding to it
}

// errPoolClosed is returned by Get after CloseAll.
var errPoolClosed = errors.New("the backend connection pool is closed")

// errNoOwner is returned by Get for a zero owner. The pinned connection is
// GetPinned's.
var errNoOwner = errors.New("a connection must have an owner; use GetPinned for the shared default")

// Opener builds a connection: the provider, and a function that closes it.
type Opener func(ctx context.Context) (MachineProvider, func() error, error)

// Get returns the provider for the target identified by key, opening it with
// open if no connection exists.
//
// owner becomes an owner of the connection, releasing whichever connection it
// owned before if that was a different one. It must not be zero. name labels
// the connection in logs and metrics.
//
// A failed open is not cached: the next Get tries again, so a hypervisor that
// was down is retried rather than remembered as broken. Concurrent callers
// share one successful dial; after a failed one, each queued caller tries for
// itself.
func (p *Pool) Get(ctx context.Context, owner types.NamespacedName, key, name string, open Opener) (MachineProvider, error) {
	if owner == (types.NamespacedName{}) {
		return nil, errNoOwner
	}
	return p.get(ctx, owner, key, name, open)
}

// GetPinned is Get for a connection that is never closed while the pool is
// open, whoever comes and goes: the shared default the manager's flags
// configure.
func (p *Pool) GetPinned(ctx context.Context, key, name string, open Opener) (MachineProvider, error) {
	return p.get(ctx, types.NamespacedName{}, key, name, open)
}

// get is Get, with a zero owner meaning pinned.
func (p *Pool) get(ctx context.Context, owner types.NamespacedName, key, name string, open Opener) (MachineProvider, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	if p.conns == nil {
		p.conns = map[string]*poolEntry{}
		p.owners = map[types.NamespacedName]string{}
	}
	e := p.conns[key]
	if e == nil {
		e = &poolEntry{name: name, owners: map[types.NamespacedName]struct{}{}, open: make(chan struct{}, 1)}
		p.conns[key] = e
	}
	// Reserved before the lock is released, so a Release racing this Get
	// cannot close the entry out from under it.
	e.pending++
	hook := p.afterReserve
	p.mu.Unlock()
	if hook != nil {
		hook()
	}

	prov, err := p.ensureOpen(ctx, e, open)

	p.mu.Lock()
	e.pending--
	var closeAfter *poolEntry
	if err == nil {
		closeAfter = p.bindLocked(owner, key, e)
	} else if p.idleLocked(e) && p.conns[key] == e {
		delete(p.conns, key)
	}
	p.mu.Unlock()
	p.closeEntry(closeAfter)
	return prov, err
}

// ensureOpen returns e's provider, opening it if it has none.
func (p *Pool) ensureOpen(ctx context.Context, e *poolEntry, open Opener) (MachineProvider, error) {
	select {
	case e.open <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-e.open }()

	p.mu.Lock()
	prov := e.prov
	p.mu.Unlock()
	if prov != nil {
		return prov, nil
	}

	prov, release, err := open(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		// Opened after CloseAll: nothing would ever close it.
		p.mu.Unlock()
		if release != nil {
			go func() { _ = release() }()
		}
		return nil, errPoolClosed
	}
	e.prov, e.release = prov, release
	metrics.ProviderConnections.Inc()
	p.mu.Unlock()
	logf.FromContext(ctx).Info("Opened hypervisor connection", "hypervisor", e.name)
	return prov, nil
}

// bindLocked makes owner an owner of e, and returns the entry owner used to own
// if that one now has nobody left and must be closed. p.mu must be held.
func (p *Pool) bindLocked(owner types.NamespacedName, key string, e *poolEntry) *poolEntry {
	if owner == (types.NamespacedName{}) {
		e.pinned = true
		return nil
	}
	previous, had := p.owners[owner]
	if had && previous == key {
		e.owners[owner] = struct{}{}
		return nil
	}
	p.owners[owner] = key
	e.owners[owner] = struct{}{}
	if !had {
		return nil
	}
	return p.unbindLocked(owner, previous)
}

// Release records that owner no longer uses any connection, and closes the one
// it used if nobody else does. Releasing an owner the pool does not know is a
// no-op, so it is safe to call for every deleted cluster.
func (p *Pool) Release(ctx context.Context, owner types.NamespacedName) {
	p.mu.Lock()
	var closeAfter *poolEntry
	if key, ok := p.owners[owner]; ok {
		delete(p.owners, owner)
		closeAfter = p.unbindLocked(owner, key)
	}
	p.mu.Unlock()
	if closeAfter != nil {
		logf.FromContext(ctx).Info("Released hypervisor connection", "hypervisor", closeAfter.name, "owner", owner.String())
	}
	p.closeEntry(closeAfter)
}

// unbindLocked removes owner from the entry at key, and returns that entry if it
// is now idle and has been removed from the pool, for the caller to close.
// p.mu must be held.
func (p *Pool) unbindLocked(owner types.NamespacedName, key string) *poolEntry {
	e := p.conns[key]
	if e == nil {
		return nil
	}
	delete(e.owners, owner)
	if !p.idleLocked(e) {
		return nil
	}
	delete(p.conns, key)
	return e
}

// idleLocked reports whether nobody uses e, holds it open, or is about to bind
// to it. p.mu must be held.
func (p *Pool) idleLocked(e *poolEntry) bool {
	return len(e.owners) == 0 && !e.pinned && e.pending == 0
}

// closeEntry closes e's connection, if it has one, in the background: a polite
// close can wait seconds on a daemon that has stopped answering, and nothing
// that releases a connection needs to wait for that.
func (p *Pool) closeEntry(e *poolEntry) {
	if e == nil {
		return
	}
	p.mu.Lock()
	release := e.release
	opened := e.prov != nil
	e.prov, e.release = nil, nil
	if opened {
		metrics.ProviderConnections.Dec()
	}
	// The hypervisor's series go with its last connection, so a cluster deleted
	// or moved does not leave dead series until the pod restarts. Two targets
	// can share a hypervisor name (same host, other credentials); while one
	// remains, its series stay.
	lastOfName := true
	for _, other := range p.conns {
		if other.name == e.name {
			lastOfName = false
			break
		}
	}
	p.mu.Unlock()
	if !opened {
		return
	}
	if lastOfName {
		metrics.ProviderOperationDuration.DeletePartialMatch(prometheus.Labels{"hypervisor": e.name})
	}
	log := logf.Log.WithName("provider-pool")
	go func() {
		if release == nil {
			return
		}
		if err := release(); err != nil {
			log.Error(err, "Failed to close hypervisor connection cleanly", "hypervisor", e.name)
			return
		}
		log.Info("Closed hypervisor connection", "hypervisor", e.name)
	}()
}

// CloseAll closes every connection the pool holds, pinned ones included, and
// waits for the closes to finish. For manager shutdown; the pool must not be
// used afterwards.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	p.closed = true
	var releases []func() error
	for key, e := range p.conns {
		if e.prov != nil {
			metrics.ProviderConnections.Dec()
			if e.release != nil {
				releases = append(releases, e.release)
			}
		}
		e.prov, e.release = nil, nil
		delete(p.conns, key)
	}
	p.owners = map[types.NamespacedName]string{}
	p.mu.Unlock()

	var wg sync.WaitGroup
	log := logf.Log.WithName("provider-pool")
	for _, release := range releases {
		wg.Go(func() {
			if err := release(); err != nil {
				log.Error(err, "Failed to close hypervisor connection cleanly at shutdown")
			}
		})
	}
	wg.Wait()
}
