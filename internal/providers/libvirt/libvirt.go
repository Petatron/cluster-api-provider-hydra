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
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// addrSourceLease selects the DHCP lease table as the address source.
//
// The generated API takes a uint32 while the constant is typed
// DomainInterfaceAddressesSource, so the conversion is required; naming it here
// keeps that out of the call site.
const addrSourceLease = uint32(golibvirt.DomainInterfaceAddressesSrcLease)

// Config describes how to reach libvirt and where to put what it creates.
type Config struct {
	// URI is a libvirt connection URI, e.g. "qemu:///system". Empty means
	// qemu:///system over the local socket.
	URI string

	// RemoteAddr, when set, dials libvirt over TCP instead of a local socket.
	// This is the case principle 9 exists for.
	RemoteAddr string

	// StoragePool is the libvirt pool that machine disks are created in.
	StoragePool string

	// BaseImage is the volume name of the backing image machines are cloned from.
	BaseImage string

	// DialTimeout bounds connection establishment. Defaults to 10s.
	DialTimeout time.Duration
}

// Provider implements providers.MachineProvider against libvirt.
type Provider struct {
	cfg Config
	lv  *golibvirt.Libvirt
}

// New connects to libvirt and returns a Provider.
//
// The caller owns the connection and must call Close.
func New(cfg Config) (*Provider, error) {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.StoragePool == "" {
		return nil, errors.New("libvirt: StoragePool is required")
	}
	if cfg.BaseImage == "" {
		return nil, errors.New("libvirt: BaseImage is required")
	}

	var lv *golibvirt.Libvirt
	if cfg.RemoteAddr != "" {
		lv = golibvirt.NewWithDialer(dialers.NewRemote(
			cfg.RemoteAddr, dialers.WithRemoteTimeout(cfg.DialTimeout)))
	} else {
		lv = golibvirt.NewWithDialer(dialers.NewLocal(
			dialers.WithLocalTimeout(cfg.DialTimeout)))
	}

	uri := golibvirt.QEMUSystem
	if cfg.URI != "" {
		uri = golibvirt.ConnectURI(cfg.URI)
	}
	if err := lv.ConnectToURI(uri); err != nil {
		return nil, fmt.Errorf("libvirt: connecting to %q: %w", uri, err)
	}
	return &Provider{cfg: cfg, lv: lv}, nil
}

// Close releases the libvirt connection.
func (p *Provider) Close() error { return p.lv.Disconnect() }

// Name implements providers.MachineProvider.
func (p *Provider) Name() string { return "libvirt" }

// Create implements providers.MachineProvider.
//
// Idempotent on spec.Name, as the interface requires: an existing domain with
// that name is returned rather than a second one being defined.
func (p *Provider) Create(ctx context.Context, spec providers.MachineSpec) (*providers.MachineState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Idempotency check first. This is the whole guarantee: a reconcile that
	// crashed after defining a domain but before recording its providerID will
	// land here and adopt the existing machine.
	if dom, err := p.lv.DomainLookupByName(spec.Name); err == nil {
		return p.stateOf(dom)
	} else if !isNotFound(err) {
		return nil, fmt.Errorf("libvirt: looking up domain %q: %w", spec.Name, err)
	}

	pool, err := p.lv.StoragePoolLookupByName(p.cfg.StoragePool)
	if err != nil {
		return nil, fmt.Errorf("libvirt: storage pool %q: %w", p.cfg.StoragePool, err)
	}

	volName := spec.Name + ".qcow2"
	if _, err := p.lv.StorageVolCreateXML(pool, volumeXML(volName, p.cfg.BaseImage, spec.DiskBytes), 0); err != nil {
		return nil, fmt.Errorf("libvirt: creating volume %q: %w", volName, err)
	}

	dom, err := p.lv.DomainDefineXML(domainXML(spec, p.cfg.StoragePool, volName))
	if err != nil {
		return nil, fmt.Errorf("libvirt: defining domain %q: %w", spec.Name, err)
	}
	if err := p.lv.DomainCreate(dom); err != nil {
		return nil, fmt.Errorf("libvirt: starting domain %q: %w", spec.Name, err)
	}
	return p.stateOf(dom)
}

// Get implements providers.MachineProvider.
func (p *Provider) Get(ctx context.Context, id string) (*providers.MachineState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dom, err := p.lookupByUUID(id)
	if err != nil {
		return nil, err
	}
	return p.stateOf(dom)
}

// Delete implements providers.MachineProvider.
//
// Deleting an absent machine succeeds, as the interface requires: teardown is
// retried, and the second attempt finding nothing is the desired end state.
func (p *Provider) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dom, err := p.lookupByUUID(id)
	if err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return nil
		}
		return err
	}

	// A running domain must be destroyed before it can be undefined. A stopped
	// one returns an error here, which is expected rather than fatal.
	if err := p.lv.DomainDestroy(dom); err != nil && !isNotFound(err) && !isInvalidState(err) {
		return fmt.Errorf("libvirt: destroying domain %q: %w", id, err)
	}

	// Remove the domain's storage along with it. Undefining without this leaves
	// orphaned qcow2 files that nothing references and nothing reclaims.
	const flags = golibvirt.DomainUndefineSnapshotsMetadata | golibvirt.DomainUndefineNvram
	if err := p.lv.DomainUndefineFlags(dom, flags); err != nil && !isNotFound(err) {
		return fmt.Errorf("libvirt: undefining domain %q: %w", id, err)
	}

	pool, err := p.lv.StoragePoolLookupByName(p.cfg.StoragePool)
	if err == nil {
		if vol, err := p.lv.StorageVolLookupByName(pool, dom.Name+".qcow2"); err == nil {
			if err := p.lv.StorageVolDelete(vol, 0); err != nil && !isNotFound(err) {
				return fmt.Errorf("libvirt: deleting volume for %q: %w", id, err)
			}
		}
	}
	return nil
}

func (p *Provider) lookupByUUID(id string) (golibvirt.Domain, error) {
	uuid, err := parseUUID(id)
	if err != nil {
		return golibvirt.Domain{}, fmt.Errorf("libvirt: %w", err)
	}
	dom, err := p.lv.DomainLookupByUUID(uuid)
	if err != nil {
		if isNotFound(err) {
			return golibvirt.Domain{}, providers.ErrNotFound
		}
		return golibvirt.Domain{}, fmt.Errorf("libvirt: looking up domain %q: %w", id, err)
	}
	return dom, nil
}

func (p *Provider) stateOf(dom golibvirt.Domain) (*providers.MachineState, error) {
	state := &providers.MachineState{ID: formatUUID(dom.UUID)}

	st, _, err := p.lv.DomainGetState(dom, 0)
	if err != nil {
		return nil, fmt.Errorf("libvirt: domain state for %q: %w", state.ID, err)
	}
	state.Ready = golibvirt.DomainState(st) == golibvirt.DomainRunning

	// Addresses come from the guest agent or the DHCP lease table, and are simply
	// absent until the guest has booted far enough to have one. That is not an
	// error -- the machine is still provisioning.
	ifaces, err := p.lv.DomainInterfaceAddresses(dom, addrSourceLease, 0)
	if err == nil {
		for _, iface := range ifaces {
			for _, addr := range iface.Addrs {
				state.Addresses = append(state.Addresses, providers.Address{
					Type:    providers.AddressTypeInternalIP,
					Address: addr.Addr,
				})
			}
		}
	}
	state.Addresses = append(state.Addresses, providers.Address{
		Type:    providers.AddressTypeHostname,
		Address: dom.Name,
	})
	return state, nil
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
