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
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Petatron/cluster-api-provider-hydra/internal/cloudinit"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// defaultRPCTimeout bounds a provider call when the caller did not set a
// deadline. DialTimeout only covers connection establishment; without this, a
// libvirt that accepts the TCP handshake and then stops answering would hold a
// reconcile worker until manager shutdown.
const defaultRPCTimeout = 30 * time.Second

// defaultConnectTimeout is DialTimeout when none was configured. It is the one
// connect default: New applies it, and tlsDialer falls back to it, so the two
// zero-value paths cannot disagree.
const defaultConnectTimeout = 10 * time.Second

// call runs a libvirt RPC under the caller's context.
//
// go-libvirt's generated API is context-free: DomainCreate and friends take no
// ctx and block until the daemon answers. That is a poor fit for a provider
// whose contract promises cancellation and whose hypervisor may be across a WAN
// hop, so every RPC goes through here.
//
// Cancelling cannot abort an in-flight RPC directly, so on timeout the
// connection is recycled. That is not tidiness: go-libvirt registers a callback
// per in-flight call and the goroutine behind it blocks until the daemon
// answers, so a daemon that accepts connections but stops replying would
// otherwise leak one goroutine and one callback per timeout, per machine,
// growing for as long as the controller runs. Dropping the connection fails
// those calls immediately and ensureConnected redials on next use.
//
// The remaining cost is a reconnect after any timeout, which is the right trade
// against unbounded growth.
func call[T any](ctx context.Context, p *Provider, fn func() (T, error)) (T, error) {
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
		p.recycle()
		var zero T
		return zero, ctx.Err()
	case r := <-ch:
		return r.val, r.err
	}
}

// callVoid is call for RPCs that return only an error.
func callVoid(ctx context.Context, p *Provider, fn func() error) error {
	_, err := call(ctx, p, func() (struct{}, error) { return struct{}{}, fn() })
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

	// DialTimeout bounds connection establishment on every transport. Defaults
	// to 10s. For TLS it covers the connect, the handshake and libvirtd's
	// certificate-verification reply together.
	DialTimeout time.Duration

	// RPCTimeout bounds each exported provider call when the caller did not
	// set a deadline. Defaults to 30s.
	RPCTimeout time.Duration
}

// client is the part of go-libvirt's API this provider calls. *golibvirt.Libvirt
// is the only production implementation; the interface exists so the failure
// paths of Create and teardown can be driven without a hypervisor, which is the
// only way to produce most of them on demand.
type client interface {
	ConnectToURI(uri golibvirt.ConnectURI) error
	Disconnect() error
	IsConnected() bool

	DomainLookupByName(name string) (golibvirt.Domain, error)
	DomainLookupByUUID(uuid golibvirt.UUID) (golibvirt.Domain, error)
	DomainDefineXML(desc string) (golibvirt.Domain, error)
	DomainIsActive(dom golibvirt.Domain) (int32, error)
	DomainGetXMLDesc(dom golibvirt.Domain, flags golibvirt.DomainXMLFlags) (string, error)
	DomainCreate(dom golibvirt.Domain) error
	DomainDestroy(dom golibvirt.Domain) error
	DomainUndefineFlags(dom golibvirt.Domain, flags golibvirt.DomainUndefineFlagsValues) error
	DomainGetState(dom golibvirt.Domain, flags uint32) (int32, int32, error)
	DomainInterfaceAddresses(dom golibvirt.Domain, source, flags uint32) ([]golibvirt.DomainInterface, error)

	ConnectListAllStoragePools(needResults int32, flags golibvirt.ConnectListAllStoragePoolsFlags) ([]golibvirt.StoragePool, uint32, error)
	StoragePoolLookupByName(name string) (golibvirt.StoragePool, error)
	StoragePoolIsActive(pool golibvirt.StoragePool) (int32, error)
	StoragePoolGetXMLDesc(pool golibvirt.StoragePool, flags golibvirt.StorageXMLFlags) (string, error)
	StorageVolCreateXML(pool golibvirt.StoragePool, desc string, flags golibvirt.StorageVolCreateFlags) (golibvirt.StorageVol, error)
	StorageVolLookupByName(pool golibvirt.StoragePool, name string) (golibvirt.StorageVol, error)
	StorageVolLookupByPath(path string) (golibvirt.StorageVol, error)
	StorageVolGetPath(vol golibvirt.StorageVol) (string, error)
	StorageVolUpload(vol golibvirt.StorageVol, r io.Reader, offset, length uint64, flags golibvirt.StorageVolUploadFlags) error
	StorageVolDelete(vol golibvirt.StorageVol, flags golibvirt.StorageVolDeleteFlags) error

	NetworkLookupByName(name string) (golibvirt.Network, error)
	NetworkDefineXML(desc string) (golibvirt.Network, error)
	NetworkIsActive(n golibvirt.Network) (int32, error)
	NetworkCreate(n golibvirt.Network) error
	NetworkSetAutostart(n golibvirt.Network, autostart int32) error
	NetworkGetXMLDesc(n golibvirt.Network, flags uint32) (string, error)
}

var _ client = (*golibvirt.Libvirt)(nil)

// Provider implements providers.MachineProvider against libvirt.
type Provider struct {
	cfg Config
	lv  client
	uri golibvirt.ConnectURI

	// mu guards the connection state below.
	mu sync.Mutex
	// dial is non-nil while a reconnect is in flight, and is closed when it
	// finishes. Concurrent callers wait on it rather than opening a second
	// connection, so a burst of reconciles against a slow hypervisor produces one
	// dial and not one per machine.
	dial    chan struct{}
	dialErr error

	// dialer is the transport, retained so a stalled connection can be dropped
	// without waiting on the daemon to acknowledge it.
	dialer *trackingDialer
}

var _ providers.MachineProvider = (*Provider)(nil)

// New connects to libvirt and returns a Provider.
//
// The caller owns the connection and must call Close.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultConnectTimeout
	}
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = defaultRPCTimeout
	}
	// StoragePool and BaseImage are both deliberately optional here, and refusing
	// to construct without them would defeat the point of HydraCluster.
	//
	// Construction happens before any object is read, so a pool required at this
	// point is required process-wide -- which would mean a cluster naming its own
	// pool still had to invent a dummy flag value just to let the manager start.
	// An unset pool is still a configuration error, it just belongs to whichever
	// cluster failed to name one: lookupPoolNamed reports it terminally, against
	// the object that can actually be fixed.

	dialer, err := newDialer(cfg)
	if err != nil {
		return nil, err
	}
	// The dial bound must exceed the handshake budget, or a slow but healthy
	// hypervisor would be cut off before it finished.
	tracked := newTrackingDialer(dialer, cfg.DialTimeout+cfg.RPCTimeout)
	lv := golibvirt.NewWithDialer(tracked)

	uri := golibvirt.QEMUSystem
	if cfg.URI != "" {
		uri = golibvirt.ConnectURI(cfg.URI)
	}

	// The initial handshake needs the same deadline as everything else.
	// ConnectToURI does not take a context, and go-libvirt's connect and
	// authentication exchange waits indefinitely for replies -- so a daemon that
	// accepts the socket and then stops answering would hang manager startup
	// forever, with no reconcile loop yet running to time out.
	//
	// DialTimeout alone does not cover this: it bounds establishing the
	// connection, not the protocol exchange that follows.
	ctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout+cfg.RPCTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- lv.ConnectToURI(uri) }()

	select {
	case <-ctx.Done():
		// Force-close rather than Disconnect. Disconnect would send a close RPC and
		// wait for a reply from the very daemon that has stopped replying, so it
		// would block here just as the handshake did.
		tracked.forceClose()
		return nil, fmt.Errorf("libvirt: connecting to %q: %w", uri, ctx.Err())
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("libvirt: connecting to %q: %w", uri, err)
		}
	}
	return &Provider{cfg: cfg, lv: lv, uri: uri, dialer: tracked}, nil
}

// Close releases the libvirt connection.
//
// Tries a graceful Disconnect, but bounded. Disconnect is a request/reply
// exchange, so against a daemon that has stopped answering it blocks -- and
// during shutdown that means hanging until Kubernetes sends SIGKILL, or
// indefinitely when run locally. The socket is force-closed if the polite
// version does not return promptly.
func (p *Provider) Close() error {
	done := make(chan error, 1)
	go func() { done <- p.lv.Disconnect() }()

	select {
	case err := <-done:
		if err != nil {
			// go-libvirt's Disconnect returns before closing its socket when the
			// close RPC itself fails, so the socket is still open here. Close it;
			// nothing else will.
			p.recycle()
		}
		return err
	case <-time.After(closeGracePeriod):
		p.recycle()
		return nil
	}
}

// closeGracePeriod is how long a polite disconnect gets before the socket is
// dropped from under it.
const closeGracePeriod = 5 * time.Second

// begin arms a deadline and reconnects if libvirtd dropped us. Every exported
// method goes through here so a stalled RPC cannot occupy a reconcile worker
// until process exit, and so a WAN blip does not require a pod restart.
func (p *Provider) begin(ctx context.Context) (context.Context, context.CancelFunc, error) {
	ctx, cancel := p.withRPCDeadline(ctx)
	if err := p.ensureConnected(ctx); err != nil {
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

// ensureConnected reconnects if libvirtd dropped us, bounded by the caller's
// context and without allowing two dials at once.
//
// The deadline matters as much here as on the RPCs it guards: a socket can
// accept and then stall during libvirt's protocol handshake, and a synchronous
// dial would block the reconcile worker just as effectively as a stalled RPC.
//
// Exactly one dial runs at a time. Callers that time out release their claim but
// leave the dial in flight, so a burst of reconciles against a slow hypervisor
// produces one connection attempt rather than one per machine.
func (p *Provider) ensureConnected(ctx context.Context) error {
	p.mu.Lock()
	if p.lv.IsConnected() {
		p.mu.Unlock()
		return nil
	}
	if p.dial == nil {
		p.dial = make(chan struct{})
		go func() {
			err := p.lv.ConnectToURI(p.uri)
			p.mu.Lock()
			p.dialErr = err
			done := p.dial
			p.dial = nil
			p.mu.Unlock()
			close(done)
		}()
	}
	done := p.dial
	p.mu.Unlock()

	select {
	case <-ctx.Done():
		// Abort the dial rather than abandoning it. Left running, p.dial stays
		// non-nil and every later provider call would join the same permanently
		// stuck attempt. Force-closing makes ConnectToURI return, so the goroutine
		// records its error and clears p.dial, letting the next call redial.
		p.dialer.forceClose()
		return fmt.Errorf("libvirt: connecting to %q: %w", p.uri, ctx.Err())
	case <-done:
		p.mu.Lock()
		err := p.dialErr
		p.mu.Unlock()
		if err != nil {
			return fmt.Errorf("libvirt: reconnecting to %q: %w", p.uri, err)
		}
		return nil
	}
}

// recycle drops the connection so RPCs parked on a stalled daemon are released.
//
// go-libvirt registers a callback for every in-flight call and the goroutine
// behind it blocks until the daemon answers. Without this, a daemon that accepts
// connections but never replies would leak one goroutine and one callback per
// timeout, per machine, growing without bound for as long as the controller
// runs. Dropping the connection fails those calls immediately; ensureConnected
// redials on the next use.
func (p *Provider) recycle() {
	// Tolerates a zero Provider so the call wrappers can be exercised without a
	// connection, and so a timeout during construction cannot panic.
	if p == nil || p.dialer == nil {
		return
	}
	// Force-close, never Disconnect. Disconnect sends a ProcConnectClose RPC and
	// waits for the reply -- from the daemon that has already stopped replying,
	// which is the only reason this is being called. It would block forever and
	// the parked RPC it was meant to release would stay parked.
	p.dialer.forceClose()
}

// Name implements providers.MachineProvider.
func (p *Provider) Name() string { return "libvirt" }

// rootVolumeName and cidataVolumeName derive a machine's volume names from its
// backend name, which is already globally unique. Both are pure functions of the
// name so that teardown can find them without a domain to consult -- the whole
// reason a crashed Create does not orphan a disk.
func rootVolumeName(machineName string) string { return machineName + ".qcow2" }

func cidataVolumeName(machineName string) string { return machineName + "-cidata.iso" }

// partitionOwnedDisks splits a domain's disks into the ones this provider
// created and the ones it did not.
//
// Deletion is keyed on the deterministic names Create uses rather than on
// whatever the domain has attached, so a volume an operator added by hand is
// never destroyed by a HydraMachine deletion.
func partitionOwnedDisks(machineName string, disks []diskSource) (ours, foreign []diskSource) {
	owned := map[string]struct{}{
		rootVolumeName(machineName):   {},
		cidataVolumeName(machineName): {},
	}
	for _, d := range disks {
		// The file name, not the whole path. Create names volumes deterministically
		// from the machine name; where the pool put them is the pool's business and
		// is not something ownership should depend on.
		if _, ok := owned[filepath.Base(d.path)]; ok {
			ours = append(ours, d)
			continue
		}
		foreign = append(foreign, d)
	}
	return ours, foreign
}

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
	//   domain defined, never started       -> check its root ports, start it
	//   domain started                      -> return it
	//
	// The middle case is the subtle one: a defined-but-inactive domain adopted
	// without being started would be polled forever, because nothing else in the
	// reconcile loop ever attempts a start.
	dom, err := call(ctx, p, func() (golibvirt.Domain, error) {
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

	// The cluster's pool when it named one, the manager's flag otherwise.
	pool, err := p.lookupPoolNamed(ctx, spec.StoragePool)
	if err != nil {
		return nil, err
	}

	backingPath, err := p.resolveBackingPath(ctx, pool, spec.Image)
	if err != nil {
		return nil, err
	}

	volName := rootVolumeName(spec.Name)
	_, err = call(ctx, p, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolCreateXML(pool, volumeXML(volName, backingPath, spec.DiskBytes), 0)
	})
	if err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("libvirt: creating volume %q: %w", volName, err)
	}
	// An already-existing volume is a previous attempt's, adopted rather than
	// treated as fatal. Without this, a crash between volume creation and domain
	// definition would make every subsequent retry fail identically forever.

	cidataVol, err := p.ensureCloudInitVolume(ctx, pool, spec)
	if err != nil {
		_ = p.deleteVolume(ctx, pool.Name, volName)
		return nil, err
	}

	// Roll back both volumes so a failure here does not leave disks that
	// FindByName cannot see. A crash in this window is recovered by
	// DeleteByName, which removes them even when no domain exists.
	// These were just created in the configured pool, so that is the right
	// place to look -- unlike deletion, which must consult the domain.
	rollback := func() {
		_ = p.deleteVolume(ctx, pool.Name, volName)
		if cidataVol != "" {
			_ = p.deleteVolume(ctx, pool.Name, cidataVol)
		}
	}

	// The domain references its disks by path, not by pool and volume, so the
	// volumes just created have to be resolved to paths first. domainXML explains
	// why that indirection is load-bearing rather than incidental.
	rootPath, err := p.volumePath(ctx, pool, volName)
	if err != nil {
		rollback()
		return nil, err
	}
	var cidataPath string
	if cidataVol != "" {
		if cidataPath, err = p.volumePath(ctx, pool, cidataVol); err != nil {
			rollback()
			return nil, err
		}
	}

	// Resolve rather than create. The cluster-level check owns this network's
	// existence, and machine creation is already gated on that check having
	// passed -- so a machine that gets this far and finds no network has hit a
	// real fault, not a race worth papering over by creating one here.
	if spec.ManagedNetwork != nil {
		bridge, nErr := p.resolveNetwork(ctx, *spec.ManagedNetwork)
		if nErr != nil {
			rollback()
			return nil, nErr
		}
		spec.Networks = append(spec.Networks, providers.Network{Name: bridge})
	}

	dom, err = call(ctx, p, func() (golibvirt.Domain, error) {
		return p.lv.DomainDefineXML(domainXML(spec, rootPath, cidataPath))
	})
	if err != nil {
		rollback()
		return nil, fmt.Errorf("libvirt: defining domain %q: %w", spec.Name, err)
	}
	if err := p.ensureRunning(ctx, dom); err != nil {
		return nil, err
	}
	return p.stateOf(ctx, dom)
}

// ensureCloudInitVolume renders the machine's bootstrap data into a NoCloud
// image and uploads it as a volume, returning the volume name.
//
// Returns an empty name when there is no bootstrap data, which is the
// no-Cluster-API case: the machine boots its base image and configures nothing.
func (p *Provider) ensureCloudInitVolume(ctx context.Context, pool golibvirt.StoragePool, spec providers.MachineSpec) (string, error) {
	if len(spec.BootstrapData) == 0 {
		return "", nil
	}

	// InstanceID is the backend name rather than anything per-boot. cloud-init
	// remembers the last instance id it saw and re-runs per-instance modules when
	// it changes -- so a value that varied would re-run the kubeadm join on every
	// restart of the machine.
	iso, err := cloudinit.ISO(cloudinit.Metadata{
		InstanceID: spec.Name,
		Hostname:   spec.Hostname,
	}, spec.BootstrapData)
	if err != nil {
		if errors.Is(err, cloudinit.ErrInvalid) {
			// Bad bootstrap data cannot be fixed by retrying, and a machine created
			// without its cloud-init would come up unconfigured and never join.
			return "", fmt.Errorf("%w: libvirt: rendering cloud-init for %q: %v", providers.ErrTerminal, spec.Name, err)
		}
		// Everything else here is the environment, not the input: staging the
		// image needs a writable temporary directory, and a full or unmounted one
		// fails now and succeeds later. Reporting that as terminal would raise a
		// condition an operator or MachineHealthCheck may remediate on.
		return "", fmt.Errorf("libvirt: rendering cloud-init for %q: %w", spec.Name, err)
	}

	volName := cidataVolumeName(spec.Name)
	vol, err := p.createCloudInitVolume(ctx, pool, volName, int64(len(iso)))
	if err != nil {
		return "", err
	}

	if err := callVoid(ctx, p, func() error {
		return p.lv.StorageVolUpload(vol, bytes.NewReader(iso), 0, uint64(len(iso)), 0)
	}); err != nil {
		// A half-written image is worse than none: cloud-init would find a
		// corrupt filesystem and the machine would boot unconfigured with nothing
		// obviously wrong. Remove it so the next attempt starts clean.
		_ = p.deleteVolume(ctx, pool.Name, volName)
		return "", fmt.Errorf("libvirt: uploading cloud-init image %q: %w", volName, err)
	}
	return volName, nil
}

// createCloudInitVolume allocates the image volume, replacing any leftover.
//
// This deliberately does NOT adopt an existing volume, which is the opposite of
// how the root disk is handled, and the difference matters. A leftover root disk
// is a valid copy-on-write clone that a previous attempt made; a leftover
// cloud-init image is by definition from an attempt that failed before defining
// a domain, so it may be truncated -- and a truncated ISO is exactly the failure
// that boots a machine which configures nothing and looks fine.
func (p *Provider) createCloudInitVolume(ctx context.Context, pool golibvirt.StoragePool, volName string, size int64) (golibvirt.StorageVol, error) {
	vol, err := call(ctx, p, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolCreateXML(pool, rawVolumeXML(volName, size), 0)
	})
	if err == nil {
		return vol, nil
	}
	if !isAlreadyExists(err) {
		return golibvirt.StorageVol{}, fmt.Errorf("libvirt: creating cloud-init volume %q: %w", volName, err)
	}

	if err := p.deleteVolume(ctx, pool.Name, volName); err != nil {
		return golibvirt.StorageVol{}, fmt.Errorf("libvirt: replacing a leftover cloud-init volume %q: %w", volName, err)
	}
	vol, err = call(ctx, p, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolCreateXML(pool, rawVolumeXML(volName, size), 0)
	})
	if err != nil {
		return golibvirt.StorageVol{}, fmt.Errorf("libvirt: recreating cloud-init volume %q: %w", volName, err)
	}
	return vol, nil
}

// ensureRunning starts a domain that is defined but not active, once its
// root ports pass startVerified's check.
func (p *Provider) ensureRunning(ctx context.Context, dom golibvirt.Domain) error {
	return startVerified(dom.Name, startSteps{
		isActive: func() (bool, error) {
			active, err := call(ctx, p, func() (int32, error) { return p.lv.DomainIsActive(dom) })
			return active == 1, err
		},
		readXML: func() (string, error) {
			return call(ctx, p, func() (string, error) {
				return p.lv.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
			})
		},
		start: func() error { return callVoid(ctx, p, func() error { return p.lv.DomainCreate(dom) }) },
	})
}

// startSteps are the three RPCs starting a domain takes. They are funcs so the
// order of check and start can be tested without a libvirt.
type startSteps struct {
	isActive func() (bool, error)
	readXML  func() (string, error)
	start    func() error
}

// startVerified starts a domain that is not running, but only after reading
// back what libvirt stored and finding no pcie-root-port with hotplug on.
//
// domainXML declares enough ports for every device it knows about. But when
// they run out, libvirt does not fail: it quietly adds ports of its own, with
// hotplug on, and the device behind one can come up missing in the guest (see
// rootPorts). That happens only if some libvirt version adds a PCIe device
// pcieDeviceCount does not count, beyond the spares.
//
// Both of Create's paths come through here, and the adopt path matters as much
// as the fresh one: an attempt that defined the domain and then lost the
// connection during the read-back leaves a domain nobody has checked, and the
// next reconcile adopts it.
//
// A refused domain is left exactly as it is, defined and never started, with
// its volumes. Nothing here undefines or deletes. Reclaiming it is Delete's job,
// which finds it by name, and a cleanup on this path would need its own
// ordering and its own context to be safe on the very failures that bring it
// here.
//
// Hotplug-on ports are terminal: the same spec renders the same XML, so a retry
// would read back the same domain. A failed read-back is not, and fails closed.
// An already-running domain is left alone; there is nothing left to prevent.
func startVerified(name string, s startSteps) error {
	active, err := s.isActive()
	if err != nil {
		return fmt.Errorf("libvirt: checking whether domain %q is active: %w", name, err)
	}
	if active {
		return nil
	}
	desc, err := s.readXML()
	if err != nil {
		return fmt.Errorf("libvirt: reading back domain XML for %q: %w", name, err)
	}
	bad, err := hotplugRootPorts(desc)
	if err != nil {
		return fmt.Errorf("libvirt: reading back domain XML for %q: %w", name, err)
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: libvirt: not starting domain %q: %d pcie-root-port(s) have hotplug on (index %s), "+
			"so libvirt attached a PCIe device beyond the ports declared for it",
			providers.ErrTerminal, name, len(bad), strings.Join(bad, ", "))
	}
	if err := s.start(); err != nil {
		return fmt.Errorf("libvirt: starting domain %q: %w", name, err)
	}
	return nil
}

// lookupPoolNamed resolves a pool by name, falling back to the configured
// default when the name is empty.
//
// The name is a parameter rather than always the configured pool for two
// reasons: deletion must target the pool a domain was actually built in rather
// than whatever is configured now, and creation must honour the pool the
// machine's cluster asked for.
//
// A pool that does not exist is configuration-shaped, not transient: retrying
// will never conjure it. Classifying it as terminal is what makes the difference
// between an operator seeing ProvisioningFailed and watching
// ProvisioningFailedRetrying scroll past forever.
func (p *Provider) lookupPoolNamed(ctx context.Context, name string) (golibvirt.StoragePool, error) {
	if name == "" {
		name = p.cfg.StoragePool
	}
	if name == "" {
		// Neither the object nor the manager named one. Terminal, and worded so
		// the reader knows both places to look -- New deliberately no longer
		// refuses to start over this, so this is where it surfaces.
		return golibvirt.StoragePool{}, fmt.Errorf(
			"%w: libvirt: no storage pool named by the HydraCluster and none configured with --libvirt-storage-pool",
			providers.ErrTerminal)
	}
	pool, err := call(ctx, p, func() (golibvirt.StoragePool, error) {
		return p.lv.StoragePoolLookupByName(name)
	})
	if err != nil {
		if isNotFound(err) {
			return golibvirt.StoragePool{}, fmt.Errorf("%w: libvirt: storage pool %q does not exist",
				providers.ErrTerminal, name)
		}
		return golibvirt.StoragePool{}, fmt.Errorf("libvirt: storage pool %q: %w", name, err)
	}
	return pool, nil
}

// resolveBackingPath turns the requested image into a backing-store path.
//
// The previous version ignored spec.Image entirely and used the process-wide
// BaseImage for every machine, so a per-machine image request was silently
// discarded. It also passed a volume *name* where libvirt expects a *path*.
func (p *Provider) resolveBackingPath(ctx context.Context, pool golibvirt.StoragePool, img providers.Image) (string, error) {
	// A URL-only image must be rejected BEFORE any default is applied. Falling
	// through would substitute the default and silently boot an image the caller
	// never asked for -- an admitted request producing the wrong machine, which
	// is worse than a clear rejection.
	//
	// Errors here name pool.Name, the pool actually queried, and not the
	// configured one. The two differ whenever a HydraCluster names its own pool,
	// and reporting the wrong one sends an operator to a resource that was never
	// involved. deleteVolume already had to learn this.
	if img.Name == "" && img.URL != "" {
		return "", fmt.Errorf("%w: libvirt: image %q must already exist in pool %q; fetching by URL is not implemented",
			providers.ErrTerminal, img.URL, pool.Name)
	}

	name := img.Name
	if name == "" {
		name = p.cfg.BaseImage
	}
	if name == "" {
		return "", fmt.Errorf("%w: libvirt: no image specified and no default base image configured", providers.ErrTerminal)
	}

	vol, err := call(ctx, p, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolLookupByName(pool, name)
	})
	if err != nil {
		if isNotFound(err) {
			return "", fmt.Errorf("%w: libvirt: image %q not found in pool %q", providers.ErrTerminal, name, pool.Name)
		}
		return "", fmt.Errorf("libvirt: looking up image %q: %w", name, err)
	}
	path, err := call(ctx, p, func() (string, error) { return p.lv.StorageVolGetPath(vol) })
	if err != nil {
		return "", fmt.Errorf("libvirt: resolving path for image %q: %w", name, err)
	}
	return path, nil
}

// volumePath returns a volume's absolute path on the host.
//
// Unlike resolveBackingPath, a missing volume here is not a configuration error
// worth reporting terminally: the caller created this volume moments ago, so its
// absence is a genuine backend fault and retrying is the right response.
func (p *Provider) volumePath(ctx context.Context, pool golibvirt.StoragePool, volName string) (string, error) {
	vol, err := call(ctx, p, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolLookupByName(pool, volName)
	})
	if err != nil {
		return "", fmt.Errorf("libvirt: looking up volume %q in pool %q: %w", volName, pool.Name, err)
	}
	path, err := call(ctx, p, func() (string, error) { return p.lv.StorageVolGetPath(vol) })
	if err != nil {
		return "", fmt.Errorf("libvirt: resolving path for volume %q: %w", volName, err)
	}
	return path, nil
}

// subnetParts splits a CIDR into the gateway address and the netmask libvirt
// wants, taking the first host address for the gateway the way libvirt's own
// networks do.
func subnetParts(cidr string) (gateway, netmask string, err error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", fmt.Errorf("%w: libvirt: subnet %q is not valid CIDR: %v",
			providers.ErrTerminal, cidr, err)
	}
	if ip.To4() == nil {
		return "", "", fmt.Errorf("%w: libvirt: subnet %q is not IPv4", providers.ErrTerminal, cidr)
	}
	// /31 and /32 are rejected rather than merely checked for a gateway address.
	// A /31 does contain a second address, so a containment check passes and the
	// network renders fine -- and then holds the gateway and nothing else, so no
	// machine can ever be given an address on it. A network that builds cleanly
	// and cannot be used is worse than one that is refused.
	ones, bits := ipnet.Mask.Size()
	if bits-ones < 2 {
		return "", "", fmt.Errorf("%w: libvirt: subnet %q leaves no addresses for machines after the gateway",
			providers.ErrTerminal, cidr)
	}
	base := ipnet.IP.To4()
	gw := make(net.IP, len(base))
	copy(gw, base)
	gw[3]++
	return gw.String(), net.IP(ipnet.Mask).String(), nil
}

// ensureNetwork creates the cluster's managed network if it is absent, and
// verifies it if it is not.
//
// Verification rather than reconciliation, deliberately. Naming an existing
// network adopts it, and an operator who built that network chose its
// addressing for reasons this provider cannot see. Quietly rewriting it would
// renumber every machine already attached; saying the declaration and the
// network disagree leaves them to decide which is wrong.
//
// Returns the bridge name, which is what machines are actually attached to.
func (p *Provider) ensureNetwork(ctx context.Context, spec providers.ManagedNetwork) (string, error) {
	return p.attachableNetwork(ctx, spec, true)
}

// resolveNetwork is ensureNetwork for machine creation: it verifies and starts
// an existing network, but never defines one.
//
// Defining the network is the cluster-level check's job, and machine creation
// is gated on that check having passed. A network missing by the time a machine
// is created was removed after the check, so the right answer is an error the
// next cluster check repairs, not a second creation path that bypasses the gate.
// Transient rather than terminal for the same reason.
func (p *Provider) resolveNetwork(ctx context.Context, spec providers.ManagedNetwork) (string, error) {
	return p.attachableNetwork(ctx, spec, false)
}

// attachableNetwork is ensureNetwork and resolveNetwork, which differ only in
// whether an absent network is defined or reported.
func (p *Provider) attachableNetwork(ctx context.Context, spec providers.ManagedNetwork, define bool) (string, error) {
	gateway, netmask, err := subnetParts(spec.Subnet)
	if err != nil {
		return "", err
	}
	existing, err := call(ctx, p, func() (golibvirt.Network, error) {
		return p.lv.NetworkLookupByName(spec.Name)
	})
	if err != nil {
		if !isNotFound(err) {
			return "", fmt.Errorf("libvirt: looking up network %q: %w", spec.Name, err)
		}
		if !define {
			return "", fmt.Errorf("libvirt: managed network %q does not exist; "+
				"the HydraCluster check creates it, and this machine waits for that", spec.Name)
		}
		return p.createNetwork(ctx, spec, gateway, netmask)
	}
	return p.verifyNetwork(ctx, existing, spec, gateway, netmask)
}

func (p *Provider) createNetwork(ctx context.Context, spec providers.ManagedNetwork, gateway, netmask string) (string, error) {
	doc, err := networkXML(spec, gateway, netmask)
	if err != nil {
		return "", err
	}
	created, err := call(ctx, p, func() (golibvirt.Network, error) {
		return p.lv.NetworkDefineXML(doc)
	})
	if err != nil {
		return "", fmt.Errorf("libvirt: defining network %q: %w", spec.Name, err)
	}
	// Nothing is rolled back if the steps below fail, because they do not need
	// to be: a network that is defined but not started is found by the next
	// reconcile, and bringUp is reached from the verify path too. That is the
	// difference between resumable and abandoned -- an earlier version reported
	// "an operator must start this" for a network it had just defined itself.
	if err := p.bringUp(ctx, created, spec.Name); err != nil {
		return "", err
	}
	return p.networkBridge(ctx, created, spec.Name)
}

// bringUp starts a network if it is not running and makes sure it comes back
// after a reboot.
//
// Both steps run every time, deliberately. Autostart in particular is only
// reachable once the network is up, so setting it once at creation means a
// failure there is silently forgotten -- the next reconcile finds a running
// network, is satisfied, and the host reboots months later into a cluster whose
// machines all have dead interfaces. Setting it on every pass costs one call.
//
// Starting a network the cluster declares is not the same liberty as rewriting
// its addressing. A declared network that is stopped makes the cluster unusable
// and starting it destroys nothing, so this ensures rather than reports --
// unlike the address and range checks in verifyNetwork, which refuse to touch
// what an operator configured.
func (p *Provider) bringUp(ctx context.Context, n golibvirt.Network, name string) error {
	active, err := call(ctx, p, func() (int32, error) { return p.lv.NetworkIsActive(n) })
	if err != nil {
		return fmt.Errorf("libvirt: checking whether network %q is running: %w", name, err)
	}
	if active == 0 {
		if err := callVoid(ctx, p, func() error { return p.lv.NetworkCreate(n) }); err != nil {
			// Something else can start the network between the check above and
			// this call. libvirt then reports operation-invalid ("network is
			// already active"), not already-exists, and the same code covers
			// faults that are real. So ask again rather than guess from the code:
			// running is the state this was after.
			again, aErr := call(ctx, p, func() (int32, error) { return p.lv.NetworkIsActive(n) })
			if aErr != nil || again != 1 {
				return fmt.Errorf("libvirt: starting network %q: %w", name, err)
			}
		}
	}
	if err := callVoid(ctx, p, func() error { return p.lv.NetworkSetAutostart(n, 1) }); err != nil {
		return fmt.Errorf("libvirt: setting autostart on network %q: %w", name, err)
	}
	return nil
}

func (p *Provider) verifyNetwork(ctx context.Context, n golibvirt.Network, spec providers.ManagedNetwork, gateway, netmask string) (string, error) {
	desc, err := call(ctx, p, func() (string, error) { return p.lv.NetworkGetXMLDesc(n, 0) })
	if err != nil {
		return "", fmt.Errorf("libvirt: reading network %q: %w", spec.Name, err)
	}
	var parsed networkDef
	if err := xml.Unmarshal([]byte(desc), &parsed); err != nil {
		return "", fmt.Errorf("libvirt: parsing network %q: %w", spec.Name, err)
	}

	// Terminal, all of it: none of these can become true by waiting, and a
	// machine attached to a network that is not the one declared would get an
	// address from a range the endpoint was chosen to avoid.
	//
	// The forward mode is checked for the same reason it is set on creation, and
	// adopting is where it matters most: a nat network whose addressing happens
	// to match would pass every other check here and then produce a cluster
	// whose API server the management controllers cannot dial. The cluster would
	// come up and be unmanageable, which is a far worse outcome than being told
	// the network is the wrong kind.
	if parsed.Forward.Mode != netForwardOpen {
		return "", fmt.Errorf("%w: libvirt: network %q has forward mode %q, but Hydra needs \"open\" -- other modes block the inbound connections the management cluster makes to a workload cluster's API server",
			providers.ErrTerminal, spec.Name, parsed.Forward.Mode)
	}
	if parsed.IP.Address != gateway || parsed.IP.Netmask != netmask {
		return "", fmt.Errorf("%w: libvirt: network %q is %s/%s, but the cluster declares %s (gateway %s)",
			providers.ErrTerminal, spec.Name, parsed.IP.Address, parsed.IP.Netmask, spec.Subnet, gateway)
	}
	if parsed.IP.DHCP == nil {
		return "", fmt.Errorf("%w: libvirt: network %q hands out no addresses, but the cluster declares a DHCP range",
			providers.ErrTerminal, spec.Name)
	}
	if parsed.IP.DHCP.Range.Start != spec.DHCPStart || parsed.IP.DHCP.Range.End != spec.DHCPEnd {
		return "", fmt.Errorf("%w: libvirt: network %q hands out %s-%s, but the cluster declares %s-%s; an endpoint chosen outside the declared range may be inside the real one",
			providers.ErrTerminal, spec.Name,
			parsed.IP.DHCP.Range.Start, parsed.IP.DHCP.Range.End, spec.DHCPStart, spec.DHCPEnd)
	}

	// Addressing is verified; liveness is ensured. See bringUp.
	if err := p.bringUp(ctx, n, spec.Name); err != nil {
		return "", err
	}
	return p.networkBridge(ctx, n, spec.Name)
}

// networkBridge reads back the bridge libvirt chose, which is the name machines
// are attached by.
func (p *Provider) networkBridge(ctx context.Context, n golibvirt.Network, name string) (string, error) {
	desc, err := call(ctx, p, func() (string, error) { return p.lv.NetworkGetXMLDesc(n, 0) })
	if err != nil {
		return "", fmt.Errorf("libvirt: reading network %q: %w", name, err)
	}
	var parsed networkDef
	if err := xml.Unmarshal([]byte(desc), &parsed); err != nil {
		return "", fmt.Errorf("libvirt: parsing network %q: %w", name, err)
	}
	if parsed.Bridge.Name == "" {
		return "", fmt.Errorf("libvirt: network %q reports no bridge to attach machines to", name)
	}
	return parsed.Bridge.Name, nil
}

// EnsureInfrastructure implements providers.MachineProvider.
//
// Two things are worth checking and nothing else is: the pool exists and is
// running, and the base image is a volume inside it. Those are exactly the
// absences that make every machine in the cluster fail, and both are one RPC.
func (p *Provider) EnsureInfrastructure(ctx context.Context, spec providers.InfrastructureSpec) error {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	pool, err := p.lookupPoolNamed(ctx, spec.StoragePool)
	if err != nil {
		return err
	}

	// Defined but not running is its own failure, and a distinct one: the pool
	// resolves, so a lookup succeeds, but every volume operation against it
	// fails. Without this check that surfaces as a confusing per-machine error.
	active, err := call(ctx, p, func() (int32, error) { return p.lv.StoragePoolIsActive(pool) })
	if err != nil {
		return fmt.Errorf("libvirt: checking whether storage pool %q is active: %w", pool.Name, err)
	}
	if active != 1 {
		// Deliberately NOT terminal, unlike a pool that does not exist. An
		// inactive pool is closer to an unreachable hypervisor: `virsh
		// pool-start`, or autostart after a libvirt restart, fixes it, and the
		// next attempt then succeeds. Terminal is the signal this codebase
		// reserves for configuration that will not fix itself, and using it here
		// would invite an operator or a MachineHealthCheck to act on something
		// about to come good on its own.
		return fmt.Errorf("libvirt: storage pool %q exists but is not running", pool.Name)
	}

	// Before the image check, not after it: the image section below returns
	// early when the cluster named no image, and a managed network is not
	// contingent on there being one.
	if spec.ManagedNetwork != nil {
		if _, err := p.ensureNetwork(ctx, *spec.ManagedNetwork); err != nil {
			return err
		}
	}

	// Only check the image when the cluster actually named one.
	//
	// A zero Image here means the HydraCluster set no default, which is a
	// supported arrangement: its machines name their own. Falling through to
	// resolveBackingPath would substitute the manager's --libvirt-base-image and
	// then demand that volume exist *inside this cluster's pool* -- gating a
	// perfectly valid cluster on an image nobody asked it to use. Machine
	// reconciliation refuses a machine whose image resolves to nothing anywhere,
	// which is where that belongs.
	if spec.Image == (providers.Image{}) {
		return nil
	}

	// resolveBackingPath already classifies a missing image as terminal, and
	// rejects a URL-only image the backend cannot fetch. Reusing it means the
	// cluster-level check and the per-machine one cannot disagree.
	if _, err := p.resolveBackingPath(ctx, pool, spec.Image); err != nil {
		return err
	}
	return nil
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

	dom, err := call(ctx, p, func() (golibvirt.Domain, error) {
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
func (p *Provider) DeleteByName(ctx context.Context, name, storagePool string) error {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	dom, err := call(ctx, p, func() (golibvirt.Domain, error) {
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
	// No domain. Create can leave volumes behind if it crashes after
	// StorageVolCreateXML and before DomainDefineXML; FindByName would report
	// not-found and the controller would release the finalizer. Removing them
	// here closes that window.
	// Search every pool, not just the configured one. Create can leave a
	// volume-only partial in pool A, and if the controller is redeployed against
	// pool B before deletion, looking only in B reports success and releases the
	// finalizer while the disk stays orphaned in A. The volume names are derived
	// from the object UID, so they are unambiguous wherever they turn up.
	//
	// Both volumes are swept, not just the root disk. Create allocates the
	// cloud-init image between the two, so a crash in that window leaves exactly
	// the ISO behind -- and a sweep that only knew about the qcow2 would report
	// success and orphan it.
	//
	// The machine's own pool is the one a leftover could be in. When the caller
	// does not know it, the pool Create would have used is the configured one.
	if storagePool == "" {
		storagePool = p.cfg.StoragePool
	}
	for _, volName := range []string{rootVolumeName(name), cidataVolumeName(name)} {
		if err := p.deleteVolumeAnyPool(ctx, volName, storagePool); err != nil {
			return err
		}
	}
	return nil
}

// deleteVolumeAnyPool removes a volume from whichever running pool holds it.
//
// Used for domain-less leftovers, where the domain XML that would normally name
// the pool no longer exists.
//
// A stopped pool cannot be searched: libvirt refuses volume calls against it,
// and its contents are unknowable. Only one stopped pool matters, though: the
// machine's own, ownPool, where Create put its volumes. If that one is stopped
// the leftover could be in it, and reporting success would release the
// finalizer over a disk nobody can find again -- so the call fails, naming it,
// and the next attempt finishes once an operator starts it.
//
// Any other stopped pool is skipped and logged. The controller sweeps after
// every deletion, so failing on those would hold every machine on the host in
// Deleting -- stalling scale-down and cluster deletion -- over a pool that may
// have nothing to do with Hydra, to guard against a leftover that could only
// be there if the pool configuration changed between Create and a crash.
func (p *Provider) deleteVolumeAnyPool(ctx context.Context, volName, ownPool string) error {
	pools, _, err := call2(ctx, p, func() ([]golibvirt.StoragePool, uint32, error) {
		return p.lv.ConnectListAllStoragePools(1, golibvirt.ConnectListStoragePoolsActive)
	})
	if err != nil {
		// Do not fall back to the configured pool. A domain-less partial can be in
		// a pool that is no longer configured, and looking only at the current one
		// would find nothing, report success, and let the controller release the
		// finalizer -- orphaning that qcow2 permanently. Propagating keeps the
		// finalizer until an exhaustive sweep is actually possible.
		return fmt.Errorf("libvirt: listing storage pools to find %q: %w", volName, err)
	}

	for _, pool := range pools {
		vol, err := call(ctx, p, func() (golibvirt.StorageVol, error) {
			return p.lv.StorageVolLookupByName(pool, volName)
		})
		if err != nil {
			// Not found covers the pool too: one undefined since the list was
			// taken has taken any way of reaching its volumes with it.
			if isNotFound(err) {
				continue
			}
			return fmt.Errorf("libvirt: looking up volume %q in pool %q: %w", volName, pool.Name, err)
		}
		if err := callVoid(ctx, p, func() error { return p.lv.StorageVolDelete(vol, 0) }); err != nil &&
			!isNotFound(err) {
			return fmt.Errorf("libvirt: deleting volume %q from pool %q: %w", volName, pool.Name, err)
		}
	}

	stopped, _, err := call2(ctx, p, func() ([]golibvirt.StoragePool, uint32, error) {
		return p.lv.ConnectListAllStoragePools(1, golibvirt.ConnectListStoragePoolsInactive)
	})
	if err != nil {
		return fmt.Errorf("libvirt: listing stopped storage pools while looking for %q: %w", volName, err)
	}
	for _, pool := range stopped {
		if ownPool != "" && pool.Name == ownPool {
			return fmt.Errorf("libvirt: storage pool %q, where %q would be, is not running; "+
				"start it so teardown can finish", pool.Name, volName)
		}
		logf.FromContext(ctx).Info("Skipping a stopped storage pool in the leftover sweep",
			"pool", pool.Name, "volume", volName, "machinePool", ownPool)
	}
	return nil
}

func (p *Provider) deleteDomain(ctx context.Context, dom golibvirt.Domain) error {
	// A running domain must be stopped before its disk can be removed. A stopped
	// one reports an invalid-state error here, which is expected rather than fatal.
	if err := callVoid(ctx, p, func() error { return p.lv.DomainDestroy(dom) }); err != nil &&
		!isNotFound(err) && !isInvalidState(err) {
		return fmt.Errorf("libvirt: destroying domain %q: %w", dom.Name, err)
	}

	// Resolve the disk from the domain itself rather than from current config.
	// The configured pool can change between the machine being created and being
	// deleted -- a redeploy with a different --libvirt-storage-pool is enough --
	// and looking in the new pool would report the volume absent, undefine the
	// domain, and strand the real disk in the old pool with nothing left pointing
	// at it. The domain records absolute paths, so deleteVolumeByPath resolves
	// each one back to whichever pool really holds it.
	disks, err := p.diskSourcesOf(ctx, dom)
	if err != nil {
		return err
	}
	// Every volume THIS PROVIDER created, not every volume attached.
	//
	// Two failure modes pull in opposite directions here. Reclaiming only the
	// first disk would leave a machine's cloud-init ISO orphaned, since a machine
	// with bootstrap data has two. Reclaiming every pool-backed disk would
	// destroy a data volume an operator attached out of band -- unrecoverably,
	// and on an operation they asked for on a different object.
	//
	// Matching the deterministic names Create uses satisfies both: the two
	// volumes Hydra made are removed, and anything else is merely detached when
	// the domain is undefined.
	ours, foreign := partitionOwnedDisks(dom.Name, disks)
	for _, d := range foreign {
		// Worth saying out loud. An operator who attached this expects it to
		// survive, and an operator who did not needs to know it was left behind.
		logf.FromContext(ctx).Info("Leaving a volume this provider did not create",
			"domain", dom.Name, "path", d.path)
	}
	for _, d := range ours {
		if err := p.deleteVolumeByPath(ctx, d.path); err != nil {
			return err
		}
	}

	const flags = golibvirt.DomainUndefineSnapshotsMetadata | golibvirt.DomainUndefineNvram
	if err := callVoid(ctx, p, func() error { return p.lv.DomainUndefineFlags(dom, flags) }); err != nil &&
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
// diskSource is one file-backed disk attached to a domain, named by its
// absolute path on the host.
//
// A path rather than a pool and volume name because that is what the domain XML
// carries -- see domainXML -- and because it makes reclamation exact: libvirt
// can find the volume that owns a path without anything having to guess which
// pool it lives in.
type diskSource struct {
	path string
}

// diskSourcesOf reads every file-backed disk out of a domain's own XML.
//
// Returns an empty slice when the domain has no such disk, which is not an
// error: there is simply nothing for this provider to reclaim.
func (p *Provider) diskSourcesOf(ctx context.Context, dom golibvirt.Domain) ([]diskSource, error) {
	desc, err := call(ctx, p, func() (string, error) {
		return p.lv.DomainGetXMLDesc(dom, 0)
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("libvirt: reading domain XML for %q: %w", dom.Name, err)
	}

	var parsed domainDef
	if err := xml.Unmarshal([]byte(desc), &parsed); err != nil {
		return nil, fmt.Errorf("libvirt: parsing domain XML for %q: %w", dom.Name, err)
	}

	var out []diskSource
	for _, d := range parsed.Devices.Disks {
		if d.Source.File == "" {
			// A disk with no file source is nothing this provider created: a
			// network-backed disk, or a CD-ROM with no medium inserted.
			continue
		}
		out = append(out, diskSource{path: d.Source.File})
	}
	return out, nil
}

// deleteVolumeByPath removes the volume a domain's disk points at.
//
// Preferred over deleteVolume during teardown because it needs no pool name.
// libvirt resolves the path to whichever pool actually contains it, so a machine
// created before --libvirt-storage-pool changed is still reclaimed from the pool
// it was really built in.
func (p *Provider) deleteVolumeByPath(ctx context.Context, path string) error {
	vol, err := call(ctx, p, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolLookupByPath(path)
	})
	if err != nil {
		if isNotFound(err) {
			return p.confirmReclaimed(ctx, path)
		}
		return fmt.Errorf("libvirt: looking up volume at %q: %w", path, err)
	}
	if err := callVoid(ctx, p, func() error { return p.lv.StorageVolDelete(vol, 0) }); err != nil &&
		!isNotFound(err) {
		return fmt.Errorf("libvirt: deleting volume at %q: %w", path, err)
	}
	return nil
}

// confirmReclaimed decides what a path lookup that found nothing means.
//
// libvirt resolves a path only against running pools. A volume in a stopped
// pool is reported as not found, exactly like one that is gone. Taking that at
// face value is how teardown used to orphan disks: the domain was undefined,
// which dropped the only record of where both volumes were, and the finalizer
// was released over files still on disk.
//
// So a miss counts as reclaimed only when no stopped pool keeps its volumes in
// the path's directory. When one does, the domain is kept, and Delete is retried
// until the pool is running again. A path in no pool libvirt knows about is
// still taken as gone: nothing here could reach it anyway, and a previous
// attempt that deleted the volume and then failed to undefine looks the same.
func (p *Provider) confirmReclaimed(ctx context.Context, volPath string) error {
	stopped, err := p.stoppedPools(ctx)
	if err != nil {
		return fmt.Errorf("libvirt: volume at %q was not found, and a stopped pool holding it cannot be ruled out: %w",
			volPath, err)
	}
	dir := filepath.Dir(volPath)
	for _, sp := range stopped {
		if sp.dir != "" && filepath.Clean(sp.dir) == dir {
			return fmt.Errorf("libvirt: storage pool %q, which holds %q, is not running; "+
				"keeping the domain until it is so its disks can be reclaimed", sp.name, volPath)
		}
	}
	return nil
}

// stoppedPool is a defined storage pool that is not running, and the directory
// it keeps its volumes in.
type stoppedPool struct {
	name string
	dir  string
}

// stoppedPools lists every defined pool that is not running. A pool's XML is
// readable whether or not it runs, which is what makes its directory knowable
// when its volumes are not.
func (p *Provider) stoppedPools(ctx context.Context) ([]stoppedPool, error) {
	pools, _, err := call2(ctx, p, func() ([]golibvirt.StoragePool, uint32, error) {
		return p.lv.ConnectListAllStoragePools(1, golibvirt.ConnectListStoragePoolsInactive)
	})
	if err != nil {
		return nil, fmt.Errorf("listing stopped storage pools: %w", err)
	}
	var out []stoppedPool
	for _, pool := range pools {
		desc, err := call(ctx, p, func() (string, error) { return p.lv.StoragePoolGetXMLDesc(pool, 0) })
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("reading storage pool %q: %w", pool.Name, err)
		}
		var def poolDef
		if err := xml.Unmarshal([]byte(desc), &def); err != nil {
			return nil, fmt.Errorf("parsing storage pool %q: %w", pool.Name, err)
		}
		out = append(out, stoppedPool{name: pool.Name, dir: def.Target.Path})
	}
	return out, nil
}

func (p *Provider) deleteVolume(ctx context.Context, poolName, volName string) error {
	pool, err := p.lookupPoolNamed(ctx, poolName)
	if err != nil {
		// A missing pool definition does not mean the qcow2 is gone: pools can
		// be undefined while their files remain on disk. Treating that as
		// success lets deletion undefine the domain and release the finalizer,
		// permanently orphaning the disk. Propagate so teardown retries once
		// the pool is restored. Do not wrap ErrTerminal: a missing pool during
		// teardown is expected to recover, and a terminal condition would invite
		// remediation of a machine that is being deleted.
		// Name the pool that was actually looked up, not the configured one. After
		// a pool configuration change those differ, and reporting the wrong one
		// sends an operator to restore a pool that was never the problem.
		return fmt.Errorf("libvirt: storage pool %q is unavailable while deleting %q: %v",
			poolName, volName, err)
	}

	vol, err := call(ctx, p, func() (golibvirt.StorageVol, error) {
		return p.lv.StorageVolLookupByName(pool, volName)
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("libvirt: looking up volume %q: %w", volName, err)
	}

	if err := callVoid(ctx, p, func() error { return p.lv.StorageVolDelete(vol, 0) }); err != nil &&
		!isNotFound(err) {
		return fmt.Errorf("libvirt: deleting volume %q: %w", volName, err)
	}
	return nil
}

func (p *Provider) lookupByUUID(ctx context.Context, id string) (golibvirt.Domain, error) {
	uuid, err := parseUUID(id)
	if err != nil {
		// Signal this distinctly. A providerID like hydra://libvirt/not-a-uuid
		// satisfies the CRD and the controller's format check but can never name a
		// domain, so treating it as an ordinary failure would wedge the finalizer
		// forever. The controller falls back to deleting by name instead.
		return golibvirt.Domain{}, fmt.Errorf("%w: libvirt: %v", providers.ErrInvalidID, err)
	}
	dom, err := call(ctx, p, func() (golibvirt.Domain, error) {
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
	state := &providers.MachineState{ID: formatUUID(dom.UUID), Name: dom.Name}

	st, _, err := call2(ctx, p, func() (int32, int32, error) { return p.lv.DomainGetState(dom, 0) })
	if err != nil {
		return nil, fmt.Errorf("libvirt: domain state for %q: %w", state.ID, err)
	}
	state.Ready = golibvirt.DomainState(st) == golibvirt.DomainRunning

	addrs, err := p.addressesOf(ctx, dom)
	if err != nil {
		return nil, err
	}
	state.Addresses = append(addrs, providers.Address{
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
func (p *Provider) addressesOf(ctx context.Context, dom golibvirt.Domain) ([]providers.Address, error) {
	for _, source := range []uint32{addrSourceAgent, addrSourceLease} {
		ifaces, err := call(ctx, p, func() ([]golibvirt.DomainInterface, error) {
			return p.lv.DomainInterfaceAddresses(dom, source, 0)
		})
		if err != nil {
			// A source being unavailable is ordinary -- no guest agent, no lease --
			// and the next source is tried. Cancellation is not: swallowing it would
			// report "no addresses yet" for a call that never actually completed,
			// and the contract requires cancellation to reach the caller.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("libvirt: querying addresses for %q: %w", dom.Name, ctx.Err())
			}
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
			return out, nil
		}
	}
	return nil, nil
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
	// IsGlobalUnicast covers this exactly: it excludes loopback, link-local,
	// multicast, unspecified and the IPv4 broadcast address, while still
	// accepting private ranges like 192.168.0.0/16 -- which is where these
	// machines actually live. The earlier hand-rolled predicate admitted
	// multicast and broadcast, and publishing either would make the controller
	// treat the machine as addressed and stop polling for a real one.
	return ip.IsGlobalUnicast()
}

// call2 is call for RPCs returning two values plus an error.
func call2[A, B any](ctx context.Context, p *Provider, fn func() (A, B, error)) (A, B, error) {
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
		p.recycle()
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
		switch golibvirt.ErrorNumber(e.Code) {
		case golibvirt.ErrStorageVolExist, golibvirt.ErrNetworkExist:
			return true
		}
	}
	return false
}

func isNotFound(err error) bool {
	var e golibvirt.Error
	if errors.As(err, &e) {
		switch golibvirt.ErrorNumber(e.Code) {
		case golibvirt.ErrNoDomain, golibvirt.ErrNoStorageVol, golibvirt.ErrNoStoragePool,
			golibvirt.ErrNoNetwork:
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
