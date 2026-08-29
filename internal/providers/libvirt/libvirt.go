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

// Package libvirt implements providers.MachineProvider on top of libvirt/KVM.
//
// It uses github.com/digitalocean/go-libvirt, which speaks libvirt's RPC wire
// protocol in pure Go. The alternative, libvirt.org/go/libvirt, is the official
// binding but requires cgo and libvirt headers at build time -- which would mean
// abandoning CGO_ENABLED=0 and the distroless/static runtime image this project
// already builds, and installing libvirt-dev on every CI runner. Speaking the
// protocol directly also serves architectural principle 9: the hypervisor may be
// remote, and a wire protocol reaches it where a local C library does not.
package libvirt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// defaultRPCTimeout bounds a provider call when the caller did not set a
// deadline. DialTimeout only covers connection establishment; without this, a
// libvirt that accepts the TCP handshake and then stops answering would hold a
// reconcile worker until manager shutdown.
const defaultRPCTimeout = 30 * time.Second

// call runs a libvirt RPC under the caller's context.
//
// go-libvirt's generated API is context-free: DomainCreate and friends take no
// ctx and block until the daemon answers. That is a poor fit for a provider
// whose contract promises cancellation and whose hypervisor may be across a WAN
// hop, so every RPC goes through here.
//
// Honest limitation: cancelling returns control to the caller but does NOT abort
// the in-flight RPC. The goroutine stays parked until the daemon replies or the
// connection drops, at which point it exits. That leaks a goroutine and a
// connection slot for the duration, which is a real cost -- but it is strictly
// better than a reconcile worker blocking indefinitely, which is what the
// previous preflight-only check allowed.
func call[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	type result struct {
		val T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		val, err := fn()
		ch <- result{val, err}
	}()

	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case r := <-ch:
		return r.val, r.err
	}
}

// callVoid is call for RPCs that return only an error.
func callVoid(ctx context.Context, fn func() error) error {
	_, err := call(ctx, func() (struct{}, error) { return struct{}{}, fn() })
	return err
}

// addrSourceLease selects the DHCP lease table as the address source.
//
// The generated API takes a uint32 while the constant is typed
// DomainInterfaceAddressesSource, so the conversion is required; naming it here
// keeps that out of the call site.
const addrSourceLease = uint32(golibvirt.DomainInterfaceAddressesSrcLease)

// addrSourceAgent asks the QEMU guest agent, which reports addresses the host's
// DHCP lease table never sees -- statically configured guests, or any guest on a
// network libvirt does not serve DHCP for.
const addrSourceAgent = uint32(golibvirt.DomainInterfaceAddressesSrcAgent)

// Config describes how to reach libvirt and where to put what it creates.
type Config struct {
	// URI is a libvirt connection URI, e.g. "qemu:///system". Empty means
	// qemu:///system over the local socket.
	URI string

	// RemoteAddr, when set, is host or host:port of a remote libvirt daemon.
	// Remote connections use TLS (libvirt's default listen, port 16514). Set
	// Insecure to use plaintext TCP instead, and only then across a trusted
	// tunnel.
	RemoteAddr string

	// Insecure dials RemoteAddr over raw TCP rather than TLS. Privileged
	// domain RPCs travel in the clear, so this is for a trusted tunnel only.
	Insecure bool

	// PKIPath is a directory containing clientcert.pem, clientkey.pem and
	// cacert.pem. Empty uses go-libvirt's default search paths.
	PKIPath string

	// StoragePool is the libvirt pool that machine disks are created in.
	StoragePool string

	// BaseImage is the volume name of the backing image machines are cloned from.
	BaseImage string

	// DialTimeout bounds connection establishment. Defaults to 10s. TLS
	// connections use go-libvirt's own dial timeout; this applies to the local
	// socket and to Insecure TCP.
	DialTimeout time.Duration

	// RPCTimeout bounds each exported provider call when the caller did not
	// set a deadline. Defaults to 30s.
	RPCTimeout time.Duration
}

// Provider implements providers.MachineProvider against libvirt.
type Provider struct {
	cfg Config
	lv  *golibvirt.Libvirt
	uri golibvirt.ConnectURI
	mu  sync.Mutex
}

var _ providers.MachineProvider = (*Provider)(nil)

// New connects to libvirt and returns a Provider.
//
// The caller owns the connection and must call Close.
func New(cfg Config) (*Provider, error) {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = defaultRPCTimeout
	}
	if cfg.StoragePool == "" {
		return nil, errors.New("libvirt: StoragePool is required")
	}
	if cfg.BaseImage == "" {
		return nil, errors.New("libvirt: BaseImage is required")
	}

	dialer, err := newDialer(cfg)
	if err != nil {
		return nil, err
	}
	lv := golibvirt.NewWithDialer(dialer)

	uri := golibvirt.QEMUSystem
	if cfg.URI != "" {
		uri = golibvirt.ConnectURI(cfg.URI)
	}
	if err := lv.ConnectToURI(uri); err != nil {
		return nil, fmt.Errorf("libvirt: connecting to %q: %w", uri, err)
	}
	return &Provider{cfg: cfg, lv: lv, uri: uri}, nil
}

// Close releases the libvirt connection.
func (p *Provider) Close() error { return p.lv.Disconnect() }

// begin arms a deadline and reconnects if libvirtd dropped us. Every exported
// method goes through here so a stalled RPC cannot occupy a reconcile worker
// until process exit, and so a WAN blip does not require a pod restart.
func (p *Provider) begin(ctx context.Context) (context.Context, context.CancelFunc, error) {
	ctx, cancel := p.withRPCDeadline(ctx)
	if err := p.ensureConnected(); err != nil {
		cancel()
		return nil, nil, err
	}
	return ctx, cancel, nil
}

func (p *Provider) withRPCDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	t := p.cfg.RPCTimeout
	if t == 0 {
		t = defaultRPCTimeout
	}
	return context.WithTimeout(ctx, t)
}

func (p *Provider) ensureConnected() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lv.IsConnected() {
		return nil
	}
	if err := p.lv.ConnectToURI(p.uri); err != nil {
		return fmt.Errorf("libvirt: reconnecting to %q: %w", p.uri, err)
	}
	return nil
}

// Name implements providers.MachineProvider.
func (p *Provider) Name() string { return "libvirt" }

// Create implements providers.MachineProvider.
//
// Idempotent on spec.Name, as the interface requires: an existing domain with
// that name is returned rather than a second one being defined.
func (p *Provider) Create(ctx context.Context, spec providers.MachineSpec) (*providers.MachineState, error) {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	// Idempotency, and more than a lookup. Every interruption point below leaves
	// a different partial state, and each one has to be recoverable:
	//
	//   volume created, domain not defined  -> adopt the volume
	//   domain defined, never started       -> start it
	//   domain started                      -> return it
	//
	// The middle case is the subtle one: a defined-but-inactive domain adopted
	// without being started would be polled forever, because nothing else in the
	// reconcile loop ever attempts a start.
	dom, err := call(ctx, func() (golibvirt.Domain, error) {
		return p.lv.DomainLookupByName(spec.Name)
	})
	switch {
	case err == nil:
		if startErr := p.ensureRunning(ctx, dom); startErr != nil {
			return nil, startErr
		}
		return p.stateOf(ctx, dom)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, err
	case !isNotFound(err):
		return nil, fmt.Errorf("libvirt: looking up domain %q: %w", spec.Name, err)
	}

	pool, err := p.lookupPool(ctx)
	if err != nil {
		return nil, err
	}

	backingPath, err := p.resolveBackingPath(ctx, pool, spec.Image)
	if err != nil {
		return nil, err
	}

	volName := spec.Name + ".qcow2"
	_, err = call(ctx, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolCreateXML(pool, volumeXML(volName, backingPath, spec.DiskBytes), 0)
	})
	if err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("libvirt: creating volume %q: %w", volName, err)
	}
	// An already-existing volume is a previous attempt's, adopted rather than
	// treated as fatal. Without this, a crash between volume creation and domain
	// definition would make every subsequent retry fail identically forever.

	dom, err = call(ctx, func() (golibvirt.Domain, error) {
		return p.lv.DomainDefineXML(domainXML(spec, p.cfg.StoragePool, volName))
	})
	if err != nil {
		// Roll back the volume so a failed define does not leave a qcow2 that
		// FindByName cannot see. A crash in this window is recovered by
		// DeleteByName, which removes the volume even when no domain exists.
		_ = p.deleteVolume(ctx, volName)
		return nil, fmt.Errorf("libvirt: defining domain %q: %w", spec.Name, err)
	}
	if err := p.ensureRunning(ctx, dom); err != nil {
		return nil, err
	}
	return p.stateOf(ctx, dom)
}

// ensureRunning starts a domain that is defined but not active.
func (p *Provider) ensureRunning(ctx context.Context, dom golibvirt.Domain) error {
	active, err := call(ctx, func() (int32, error) { return p.lv.DomainIsActive(dom) })
	if err != nil {
		return fmt.Errorf("libvirt: checking whether domain %q is active: %w", dom.Name, err)
	}
	if active == 1 {
		return nil
	}
	if err := callVoid(ctx, func() error { return p.lv.DomainCreate(dom) }); err != nil {
		return fmt.Errorf("libvirt: starting domain %q: %w", dom.Name, err)
	}
	return nil
}

// lookupPool resolves the configured storage pool.
//
// A pool that does not exist is configuration-shaped, not transient: retrying
// will never conjure it. Classifying it as terminal is what makes the difference
// between an operator seeing ProvisioningFailed and watching
// ProvisioningFailedRetrying scroll past forever.
func (p *Provider) lookupPool(ctx context.Context) (golibvirt.StoragePool, error) {
	pool, err := call(ctx, func() (golibvirt.StoragePool, error) {
		return p.lv.StoragePoolLookupByName(p.cfg.StoragePool)
	})
	if err != nil {
		if isNotFound(err) {
			return golibvirt.StoragePool{}, fmt.Errorf("%w: libvirt: storage pool %q does not exist",
				providers.ErrTerminal, p.cfg.StoragePool)
		}
		return golibvirt.StoragePool{}, fmt.Errorf("libvirt: storage pool %q: %w", p.cfg.StoragePool, err)
	}
	return pool, nil
}

// resolveBackingPath turns the requested image into a backing-store path.
//
// The previous version ignored spec.Image entirely and used the process-wide
// BaseImage for every machine, so a per-machine image request was silently
// discarded. It also passed a volume *name* where libvirt expects a *path*.
func (p *Provider) resolveBackingPath(ctx context.Context, pool golibvirt.StoragePool, img providers.Image) (string, error) {
	// A URL-only image must be rejected BEFORE the default is applied. Because
	// New requires BaseImage, falling through would substitute the default and
	// silently boot an image the caller never asked for -- an admitted request
	// producing the wrong machine, which is worse than a clear rejection.
	if img.Name == "" && img.URL != "" {
		return "", fmt.Errorf("%w: libvirt: image %q must already exist in pool %q; fetching by URL is not implemented",
			providers.ErrTerminal, img.URL, p.cfg.StoragePool)
	}

	name := img.Name
	if name == "" {
		name = p.cfg.BaseImage
	}
	if name == "" {
		return "", fmt.Errorf("%w: libvirt: no image specified and no default base image configured", providers.ErrTerminal)
	}

	vol, err := call(ctx, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolLookupByName(pool, name)
	})
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("%w: libvirt: image %q not found in pool %q", providers.ErrTerminal, name, p.cfg.StoragePool)
		}
		return "", fmt.Errorf("libvirt: looking up image %q: %w", name, err)
	}
	path, err := call(ctx, func() (string, error) { return p.lv.StorageVolGetPath(vol) })
	if err != nil {
		return "", fmt.Errorf("libvirt: resolving path for image %q: %w", name, err)
	}
	return path, nil
}

// Get implements providers.MachineProvider.
func (p *Provider) Get(ctx context.Context, id string) (*providers.MachineState, error) {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	dom, err := p.lookupByUUID(ctx, id)
	if err != nil {
		return nil, err
	}
	return p.stateOf(ctx, dom)
}

// FindByName implements providers.MachineProvider.
//
// Used when a machine exists but its providerID was never persisted, so the
// name is the only handle the controller has left.
func (p *Provider) FindByName(ctx context.Context, name string) (*providers.MachineState, error) {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	dom, err := call(ctx, func() (golibvirt.Domain, error) {
		return p.lv.DomainLookupByName(name)
	})
	if err != nil {
		if isNotFound(err) {
			return nil, providers.ErrNotFound
		}
		return nil, fmt.Errorf("libvirt: looking up domain %q: %w", name, err)
	}
	return p.stateOf(ctx, dom)
}

// Delete implements providers.MachineProvider.
//
// Deleting an absent machine succeeds: teardown is retried, and the second
// attempt finding nothing is the desired end state.
//
// Order matters. Storage is removed BEFORE the domain is undefined, because the
// domain is the only handle by which a retry can find the volume again. Undefining
// first and then failing on storage would orphan the qcow2 permanently -- the next
// attempt looks up the UUID, finds nothing, and returns success while the disk
// stays on the host forever.
func (p *Provider) Delete(ctx context.Context, id string) error {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	dom, err := p.lookupByUUID(ctx, id)
	if err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return nil
		}
		return err
	}
	return p.deleteDomain(ctx, dom)
}

// DeleteByName implements providers.MachineProvider.
func (p *Provider) DeleteByName(ctx context.Context, name string) error {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	dom, err := call(ctx, func() (golibvirt.Domain, error) {
		return p.lv.DomainLookupByName(name)
	})
	switch {
	case err == nil:
		return p.deleteDomain(ctx, dom)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case !isNotFound(err):
		return fmt.Errorf("libvirt: looking up domain %q: %w", name, err)
	}
	// No domain. Create can leave a clone volume behind if it crashes after
	// StorageVolCreateXML and before DomainDefineXML; FindByName would report
	// not-found and the controller would release the finalizer. Removing the
	// volume here closes that window.
	return p.deleteVolume(ctx, name+".qcow2")
}

func (p *Provider) deleteDomain(ctx context.Context, dom golibvirt.Domain) error {
	// A running domain must be stopped before its disk can be removed. A stopped
	// one reports an invalid-state error here, which is expected rather than fatal.
	if err := callVoid(ctx, func() error { return p.lv.DomainDestroy(dom) }); err != nil &&
		!isNotFound(err) && !isInvalidState(err) {
		return fmt.Errorf("libvirt: destroying domain %q: %w", dom.Name, err)
	}

	if err := p.deleteVolume(ctx, dom.Name+".qcow2"); err != nil {
		return err
	}

	const flags = golibvirt.DomainUndefineSnapshotsMetadata | golibvirt.DomainUndefineNvram
	if err := callVoid(ctx, func() error { return p.lv.DomainUndefineFlags(dom, flags) }); err != nil &&
		!isNotFound(err) {
		return fmt.Errorf("libvirt: undefining domain %q: %w", dom.Name, err)
	}
	return nil
}

// deleteVolume removes a machine's disk, propagating anything that is not a
// benign "already gone".
//
// The previous version swallowed pool and volume lookup errors entirely, so a
// transient failure here looked like success and left the disk behind.
func (p *Provider) deleteVolume(ctx context.Context, volName string) error {
	pool, err := p.lookupPool(ctx)
	if err != nil {
		// A missing pool definition does not mean the qcow2 is gone: pools can
		// be undefined while their files remain on disk. Treating that as
		// success lets deletion undefine the domain and release the finalizer,
		// permanently orphaning the disk. Propagate so teardown retries once
		// the pool is restored. Do not wrap ErrTerminal: a missing pool during
		// teardown is expected to recover, and a terminal condition would invite
		// remediation of a machine that is being deleted.
		return fmt.Errorf("libvirt: storage pool %q is unavailable while deleting %q: %v",
			p.cfg.StoragePool, volName, err)
	}

	vol, err := call(ctx, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolLookupByName(pool, volName)
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("libvirt: looking up volume %q: %w", volName, err)
	}

	if err := callVoid(ctx, func() error { return p.lv.StorageVolDelete(vol, 0) }); err != nil &&
		!isNotFound(err) {
		return fmt.Errorf("libvirt: deleting volume %q: %w", volName, err)
	}
	return nil
}

func (p *Provider) lookupByUUID(ctx context.Context, id string) (golibvirt.Domain, error) {
	uuid, err := parseUUID(id)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("libvirt: %w", err)
	}
	dom, err := call(ctx, func() (golibvirt.Domain, error) {
		return p.lv.DomainLookupByUUID(uuid)
	})
	if err != nil {
		if isNotFound(err) {
			return golibvirt.Domain{}, providers.ErrNotFound
		}
		return golibvirt.Domain{}, fmt.Errorf("libvirt: looking up domain %q: %w", id, err)
	}
	return dom, nil
}

func (p *Provider) stateOf(ctx context.Context, dom golibvirt.Domain) (*providers.MachineState, error) {
	state := &providers.MachineState{ID: formatUUID(dom.UUID)}

	st, _, err := call2(ctx, func() (int32, int32, error) { return p.lv.DomainGetState(dom, 0) })
	if err != nil {
		return nil, fmt.Errorf("libvirt: domain state for %q: %w", state.ID, err)
	}
	state.Ready = golibvirt.DomainState(st) == golibvirt.DomainRunning

	state.Addresses = p.addressesOf(ctx, dom)
	state.Addresses = append(state.Addresses, providers.Address{
		Type:    providers.AddressTypeHostname,
		Address: dom.Name,
	})
	return state, nil
}

// addressesOf asks the guest agent first and falls back to the DHCP lease table.
//
// Order matters: the domain XML installs a QEMU guest agent channel precisely so
// addresses can be read from inside the guest. Querying only leases -- as the
// first version did -- made that channel dead weight and meant a guest with a
// static address, or on a network libvirt does not serve DHCP for, would never
// report one.
//
// Absent addresses are not an error. A machine that has not finished booting
// simply has none yet.
func (p *Provider) addressesOf(ctx context.Context, dom golibvirt.Domain) []providers.Address {
	for _, source := range []uint32{addrSourceAgent, addrSourceLease} {
		ifaces, err := call(ctx, func() ([]golibvirt.DomainInterface, error) {
			return p.lv.DomainInterfaceAddresses(dom, source, 0)
		})
		if err != nil {
			continue
		}
		var out []providers.Address
		for _, iface := range ifaces {
			for _, addr := range iface.Addrs {
				if !isRoutable(addr.Addr) {
					continue
				}
				out = append(out, providers.Address{
					Type:    providers.AddressTypeInternalIP,
					Address: addr.Addr,
				})
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// isRoutable rejects addresses that say nothing about how to reach a machine.
//
// The guest agent reports every interface it can see, including loopback and
// IPv6 link-local. Publishing those as InternalIP is actively harmful: the
// controller treats any IP as "addressed" and drops to a slow health interval,
// so a loopback arriving before the real NIC would freeze status at 127.0.0.1.
func isRoutable(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

// call2 is call for RPCs returning two values plus an error.
func call2[A, B any](ctx context.Context, fn func() (A, B, error)) (A, B, error) {
	type result struct {
		a   A
		b   B
		err error
	}
	ch := make(chan result, 1)
	go func() {
		a, b, err := fn()
		ch <- result{a, b, err}
	}()
	select {
	case <-ctx.Done():
		var za A
		var zb B
		return za, zb, ctx.Err()
	case r := <-ch:
		return r.a, r.b, r.err
	}
}

func isAlreadyExists(err error) bool {
	var e golibvirt.Error
	if errors.As(err, &e) {
		return golibvirt.ErrorNumber(e.Code) == golibvirt.ErrStorageVolExist
	}
	return false
}

func isNotFound(err error) bool {
	var e golibvirt.Error
	if errors.As(err, &e) {
		switch golibvirt.ErrorNumber(e.Code) {
		case golibvirt.ErrNoDomain, golibvirt.ErrNoStorageVol, golibvirt.ErrNoStoragePool:
			return true
		}
	}
	return false
}

func isInvalidState(err error) bool {
	var e golibvirt.Error
	if errors.As(err, &e) {
		return golibvirt.ErrorNumber(e.Code) == golibvirt.ErrOperationInvalid
	}
	return false
}
