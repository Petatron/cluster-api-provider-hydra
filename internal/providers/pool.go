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
// Owners are what make closing possible. An object that resolves its provider
// becomes an owner of that target's connection; when it moves to another
// target, or is gone, it releases the old one, and the last owner to leave
// closes it. A connection obtained with GetPinned is never closed: that is the
// manager-wide connection configured by flags, which clusters naming no
// hypervisor share, exactly as before the pool existed. Pinning is explicit
// rather than implied by a missing owner, so a caller that fails to produce an
// owner gets an error instead of a connection nothing will ever close.
//
// The pool also owns the hypervisor metric label. Each target gets one when it
// enters the pool and gives it back when it leaves, and a label's series are
// dropped when its last target leaves. Only maxHypervisorLabels hypervisors get
// a label of their own; past that they share OtherHypervisor, so whatever
// clusters name, the label stays bounded.
//
// The zero value is ready to use.
type Pool struct {
	mu     sync.Mutex
	conns  map[string]*poolEntry
	owners map[Owner]string
	// labels counts the entries using each hypervisor label value.
	labels map[string]int
	// closed is set by CloseAll; the pool refuses all work afterwards.
	closed bool
	// getting counts, per owner, the Gets in flight and the Releases that
	// landed while they were, so a Get that a Release overtook does not bind
	// the owner again afterwards. An owner is in it only while a Get is.
	getting map[Owner]*ownerGets
	// closes counts connections being closed in the background, so CloseAll
	// can wait for the ones a Release or a move started too. Added to only
	// under mu, by removeLocked, and nothing removes an entry once CloseAll has
	// emptied the pool, so no Add runs concurrently with CloseAll's Wait.
	closes sync.WaitGroup

	// afterReserve, when set, runs in Get between reserving an entry and
	// binding to it -- the window a concurrent Release must not close the entry
	// in. Tests only.
	afterReserve func()
	// beforeBind, when set, runs in Get between opening and binding. Tests only.
	beforeBind func()
	// labelLimit overrides maxHypervisorLabels. Tests only.
	labelLimit int
}

// Owner is what holds a connection: an object, named with its kind, because a
// HydraCluster and a HydraMachine may share a namespace and name.
type Owner struct {
	Kind string
	types.NamespacedName
}

func (o Owner) String() string { return o.Kind + " " + o.NamespacedName.String() }

// maxHypervisorLabels is how many hypervisors a pool labels by name at once.
// Generous for any real site, and it bounds the series an operator can create
// by naming hypervisors to at most this many plus OtherHypervisor.
// internal/metrics, docs/metrics.md and docs/alerts.md state the number; change
// them with it.
const maxHypervisorLabels = 16

// OtherHypervisor is the hypervisor label shared by every target that entered
// the pool while maxHypervisorLabels others were labelled by name. It keeps the
// label until it leaves the pool, even if a slot frees up meanwhile. A
// hypervisor whose name is literally "other" shares it too.
const OtherHypervisor = "other"

// poolEntry is one target's connection.
type poolEntry struct {
	name string
	// label is name, or OtherHypervisor; fixed for the entry's life.
	label string

	// open serialises opening, so concurrent callers for one target share one
	// dial instead of racing several. A channel rather than a mutex, so a
	// caller queued behind someone else's dial can give up when its own
	// context ends instead of waiting out that dial's whole timeout.
	open chan struct{}

	// Guarded by Pool.mu.
	prov    MachineProvider
	release func() error
	owners  map[Owner]struct{}
	pinned  bool // obtained with GetPinned: never closed
	pending int  // callers between reserving this entry and binding to it
}

// errPoolClosed is returned by Get after CloseAll.
var errPoolClosed = errors.New("the backend connection pool is closed")

// errNoOwner is returned by Get for an owner with no kind or name. The pinned
// connection is GetPinned's.
var errNoOwner = errors.New("a connection must have an owner; use GetPinned for the shared default")

// errOwnerReleased is returned by a Get whose owner was released while it was
// in flight. The owner is not bound; a caller that still needs the connection
// asks again.
var errOwnerReleased = errors.New("the owner was released while its connection was being resolved")

// ownerGets is one owner's entry in Pool.getting.
type ownerGets struct {
	inFlight int // Gets for this owner between reserving and binding
	releases int // Releases of this owner since the first of them began
}

// Opener builds a connection: the provider, and a function that closes it.
// hypervisor is the metric label its samples go under, which the pool chooses:
// the target's name, or OtherHypervisor.
type Opener func(ctx context.Context, hypervisor string) (MachineProvider, func() error, error)

// Get returns the provider for the target identified by key, opening it with
// open if no connection exists.
//
// owner becomes an owner of the target, releasing whichever target it owned
// before if that was a different one. It must have a kind and a name. name
// labels the connection in logs and metrics.
//
// A failed open is not cached: the next Get tries again, so a hypervisor that
// was down is retried rather than remembered as broken. The owner is bound all
// the same, because it still wants this target: that keeps the target, and its
// hypervisor's series, in the pool until the owner leaves, which is what lets
// HydraBackendUnreachable see a hypervisor that has never answered. Concurrent
// callers share one successful dial; after a failed one, each queued caller
// tries for itself.
//
// A Release of owner that lands while this Get is in flight wins: the Get
// returns errOwnerReleased rather than binding the owner again afterwards,
// which would leave a deleted object holding a connection nobody releases.
func (p *Pool) Get(ctx context.Context, owner Owner, key, name string, open Opener) (MachineProvider, error) {
	if owner.Kind == "" || owner.Name == "" {
		return nil, errNoOwner
	}
	return p.get(ctx, owner, key, name, open)
}

// GetPinned is Get for a connection that is never closed while the pool is
// open, whoever comes and goes: the shared default the manager's flags
// configure.
func (p *Pool) GetPinned(ctx context.Context, key, name string, open Opener) (MachineProvider, error) {
	return p.get(ctx, Owner{}, key, name, open)
}

// get is Get, with a zero owner meaning pinned.
func (p *Pool) get(ctx context.Context, owner Owner, key, name string, open Opener) (MachineProvider, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	if p.conns == nil {
		p.conns = map[string]*poolEntry{}
		p.owners = map[Owner]string{}
		p.labels = map[string]int{}
		p.getting = map[Owner]*ownerGets{}
	}
	e := p.conns[key]
	created := e == nil
	if created {
		e = &poolEntry{name: name, label: p.acquireLabelLocked(name), owners: map[Owner]struct{}{}, open: make(chan struct{}, 1)}
		p.conns[key] = e
	}
	// Reserved before the lock is released, so a Release racing this Get
	// cannot close the entry out from under it.
	e.pending++
	og, releasesBefore := p.startGetLocked(owner)
	hook, beforeBind := p.afterReserve, p.beforeBind
	p.mu.Unlock()
	if created && e.label != name {
		logf.FromContext(ctx).Info("Labelled hypervisor metrics as other: too many hypervisors to label each",
			"hypervisor", name, "limit", p.limit())
	}
	if hook != nil {
		hook()
	}

	prov, err := p.ensureOpen(ctx, e, open)
	if beforeBind != nil {
		beforeBind()
	}

	p.mu.Lock()
	e.pending--
	overtaken := p.endGetLocked(owner, og, releasesBefore)
	if p.closed {
		// CloseAll ran while this was opening. It has closed whatever was open
		// and taken the entry out, so prov, if any, is already closed.
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	if overtaken {
		// Its owner was released meanwhile: bind nothing, and close what this
		// Get opened if nobody else wants it.
		var c *closing
		if p.idleLocked(e) {
			c = p.removeLocked(key, e)
		}
		p.mu.Unlock()
		p.closeInBackground(c)
		return nil, errOwnerReleased
	}
	// Bound even after a failed open; see Get. Nothing can have taken e out of
	// the pool meanwhile: only CloseAll removes an entry someone has reserved.
	c := p.bindLocked(owner, key, e)
	p.mu.Unlock()
	p.closeInBackground(c)
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
	prov, closed := e.prov, p.closed
	p.mu.Unlock()
	if closed {
		// Queued behind a dial that CloseAll overtook: do not start another.
		return nil, errPoolClosed
	}
	if prov != nil {
		return prov, nil
	}

	prov, release, err := open(ctx, e.label)
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

// bindLocked makes owner an owner of e, and returns the target owner used to
// own if that one now has nobody left and has been taken out of the pool.
// p.mu must be held.
func (p *Pool) bindLocked(owner Owner, key string, e *poolEntry) *closing {
	if owner == (Owner{}) {
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
// no-op, so it is safe to call for every deleted object.
func (p *Pool) Release(ctx context.Context, owner Owner) {
	p.mu.Lock()
	if og := p.getting[owner]; og != nil {
		og.releases++
	}
	var c *closing
	if key, ok := p.owners[owner]; ok {
		delete(p.owners, owner)
		c = p.unbindLocked(owner, key)
	}
	p.mu.Unlock()
	if c != nil {
		logf.FromContext(ctx).Info("Released hypervisor connection", "hypervisor", c.name, "owner", owner.String())
	}
	p.closeInBackground(c)
}

// unbindLocked removes owner from the target at key, and takes the target out
// of the pool if that left it idle. p.mu must be held.
func (p *Pool) unbindLocked(owner Owner, key string) *closing {
	e := p.conns[key]
	if e == nil {
		return nil
	}
	delete(e.owners, owner)
	if !p.idleLocked(e) {
		return nil
	}
	return p.removeLocked(key, e)
}

// idleLocked reports whether nobody uses e, holds it open, or is about to bind
// to it. p.mu must be held.
func (p *Pool) idleLocked(e *poolEntry) bool {
	return len(e.owners) == 0 && !e.pinned && e.pending == 0
}

// closing is a target taken out of the pool, whose connection, if it had one,
// is closed outside the pool's lock.
type closing struct {
	name    string
	release func() error
}

// removeLocked takes e out of the pool, with its connection and its share of
// its hypervisor label. The caller must pass the result to closeInBackground.
// p.mu must be held.
//
// A label's series are dropped here, under the lock, so no Get can take the
// label up again between deciding it is unused and dropping them. A call
// already in flight on the closed connection can still record one last sample
// afterwards, but only for an owner released while its own calls are running.
// An object is released from its own reconcile, once that is finished with the
// connection, and controller-runtime never runs two reconciles of one object at
// once.
func (p *Pool) removeLocked(key string, e *poolEntry) *closing {
	delete(p.conns, key)
	c := &closing{name: e.name, release: e.release}
	if c.release != nil {
		p.closes.Add(1) // done by closeInBackground, which every caller runs
	}
	if e.prov != nil {
		metrics.ProviderConnections.Dec()
	}
	e.prov, e.release = nil, nil
	if p.labels[e.label]--; p.labels[e.label] <= 0 {
		delete(p.labels, e.label)
		metrics.ProviderOperationDuration.DeletePartialMatch(prometheus.Labels{"hypervisor": e.label})
	}
	return c
}

// acquireLabelLocked returns the hypervisor label for a new target named name,
// and counts the target as using it. p.mu must be held.
func (p *Pool) acquireLabelLocked(name string) string {
	label := name
	if p.labels[name] == 0 && p.namedLabelsLocked() >= p.limit() {
		label = OtherHypervisor
	}
	p.labels[label]++
	return label
}

// namedLabelsLocked is how many hypervisors have a label of their own.
// p.mu must be held.
func (p *Pool) namedLabelsLocked() int {
	n := len(p.labels)
	if p.labels[OtherHypervisor] > 0 {
		n--
	}
	return n
}

func (p *Pool) limit() int {
	if p.labelLimit > 0 {
		return p.labelLimit
	}
	return maxHypervisorLabels
}

// closeInBackground closes c's connection, if it had one, without waiting: a
// polite close can wait seconds on a daemon that has stopped answering, and
// nothing that releases a connection needs to wait for that. CloseAll does.
func (p *Pool) closeInBackground(c *closing) {
	if c == nil || c.release == nil {
		return
	}
	log := logf.Log.WithName("provider-pool")
	go func() {
		defer p.closes.Done()
		if err := c.release(); err != nil {
			log.Error(err, "Failed to close hypervisor connection cleanly", "hypervisor", c.name)
			return
		}
		log.Info("Closed hypervisor connection", "hypervisor", c.name)
	}()
}

// startGetLocked records a Get for owner as in flight, and returns how many
// Releases of owner had landed by then. Nothing for the pinned owner, which is
// never released. p.mu must be held.
func (p *Pool) startGetLocked(owner Owner) (*ownerGets, int) {
	if owner == (Owner{}) {
		return nil, 0
	}
	og := p.getting[owner]
	if og == nil {
		og = &ownerGets{}
		p.getting[owner] = og
	}
	og.inFlight++
	return og, og.releases
}

// endGetLocked records that Get as finished, and reports whether a Release of
// its owner landed while it was in flight. p.mu must be held.
func (p *Pool) endGetLocked(owner Owner, og *ownerGets, releasesBefore int) bool {
	if og == nil {
		return false
	}
	og.inFlight--
	if og.inFlight == 0 && p.getting[owner] == og {
		delete(p.getting, owner)
	}
	return og.releases != releasesBefore
}

// CloseAll closes every connection the pool holds, pinned ones included, and
// waits for those closes to finish, and for any a Release or a move had already
// started. A dial still in flight is not waited for: its connection is closed in
// the background once it completes. For manager shutdown; the pool must not be
// used afterwards.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	p.closed = true
	cs := make([]*closing, 0, len(p.conns))
	for key, e := range p.conns {
		cs = append(cs, p.removeLocked(key, e))
	}
	p.owners = map[Owner]string{}
	p.mu.Unlock()

	for _, c := range cs {
		p.closeInBackground(c)
	}
	p.closes.Wait()
}
