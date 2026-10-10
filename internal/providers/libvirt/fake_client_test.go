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
	"encoding/xml"
	"io"
	"path"
	"slices"
	"sync"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
)

const (
	testPool  = "k8s-workers"
	testImage = "ubuntu-24.04"
)

// newFakeProvider returns a Provider wired to a fake hypervisor holding one
// active pool, which contains the base image testSpec asks for.
func newFakeProvider(t *testing.T) (*Provider, *fakeLibvirt) {
	t.Helper()
	f := newFakeLibvirt()
	f.addPool(testPool, testImage)
	p := &Provider{
		cfg: Config{StoragePool: testPool, BaseImage: testImage, RPCTimeout: 5 * time.Second},
		lv:  f,
	}
	return p, f
}

// fakeLibvirt is an in-memory hypervisor behind the client interface.
//
// It is stateful rather than a table of canned replies, so a test can assert
// what is left on the host after a call -- which volumes survived, whether the
// domain is still defined -- and not only which RPCs were made. Teardown bugs
// are about end state, and a canned reply cannot show one.
type fakeLibvirt struct {
	mu sync.Mutex

	connected  bool
	connectErr error
	// dial, when set, runs inside ConnectToURI outside the lock, standing in for
	// the socket go-libvirt dials; its error fails the connect.
	dial func() error
	// isConnectedCalls counts IsConnected, which every provider call makes
	// before it decides whether to dial.
	isConnectedCalls int

	pools    map[string]*fakePool
	domains  map[string]*fakeDomain
	networks map[string]*fakeNetwork

	// errs injects a failure. A key is either a method name, failing every call
	// to it, or "Method:arg", failing only calls naming that object.
	errs map[string]error

	// hook, when set, runs at the start of every call outside the lock, so a
	// test can block a call or change state underneath it.
	hook func(method string)

	calls    []string
	nextUUID byte
}

type fakePool struct {
	name   string
	dir    string
	active bool
	vols   map[string]*fakeVol
	// xml, when set, is what StoragePoolGetXMLDesc returns instead of a dir
	// pool rooted at dir.
	xml string
}

type fakeVol struct {
	xml  string
	data []byte
}

type fakeDomain struct {
	dom    golibvirt.Domain
	xml    string
	active bool
	// addrs is what DomainInterfaceAddresses reports per source; a source with
	// no entry fails, the way an absent guest agent does.
	addrs map[uint32][]golibvirt.DomainInterface
}

type fakeNetwork struct {
	net       golibvirt.Network
	xml       string
	active    bool
	autostart bool
}

func newFakeLibvirt() *fakeLibvirt {
	return &fakeLibvirt{
		connected: true,
		pools:     map[string]*fakePool{},
		domains:   map[string]*fakeDomain{},
		networks:  map[string]*fakeNetwork{},
		errs:      map[string]error{},
	}
}

func lvErr(code golibvirt.ErrorNumber) error {
	return golibvirt.Error{Code: uint32(code), Message: "synthetic " + code.String()}
}

// addPool defines an active pool whose volumes live under /pools/<name>.
func (f *fakeLibvirt) addPool(name string, vols ...string) *fakePool {
	p := &fakePool{name: name, dir: "/pools/" + name, active: true, vols: map[string]*fakeVol{}}
	for _, v := range vols {
		p.vols[v] = &fakeVol{}
	}
	f.pools[name] = p
	return p
}

func (f *fakeLibvirt) addDomain(name, desc string, active bool) *fakeDomain {
	f.nextUUID++
	d := &fakeDomain{
		dom:    golibvirt.Domain{Name: name, UUID: golibvirt.UUID{15: f.nextUUID}},
		xml:    desc,
		active: active,
		addrs:  map[uint32][]golibvirt.DomainInterface{},
	}
	f.domains[name] = d
	return d
}

func (f *fakeLibvirt) addNetwork(name string, def networkDef, active bool) *fakeNetwork {
	out, err := xml.Marshal(def)
	if err != nil {
		panic(err)
	}
	n := &fakeNetwork{net: golibvirt.Network{Name: name}, xml: string(out), active: active}
	f.networks[name] = n
	return n
}

func (f *fakeLibvirt) hasVol(pool, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pools[pool]
	if !ok {
		return false
	}
	_, ok = p.vols[name]
	return ok
}

// stall makes every call to method hang until the test ends, the way an RPC to
// a daemon that has stopped answering does.
func (f *fakeLibvirt) stall(t *testing.T, method string) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.hook = func(m string) {
		if m == method {
			<-release
		}
	}
}

func (f *fakeLibvirt) connectedChecks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isConnectedCalls
}

func (f *fakeLibvirt) called(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

// enter records a call and returns any failure injected for it. The caller
// holds f.mu on return.
func (f *fakeLibvirt) enter(method, arg string) error {
	if h := f.hook; h != nil {
		h(method)
	}
	f.mu.Lock()
	f.calls = append(f.calls, method)
	if err, ok := f.errs[method+":"+arg]; ok {
		return err
	}
	return f.errs[method]
}

func (f *fakeLibvirt) ConnectToURI(golibvirt.ConnectURI) error {
	err := f.enter("ConnectToURI", "")
	dial := f.dial
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if dial != nil {
		if err := dial(); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connectErr != nil {
		return f.connectErr
	}
	f.connected = true
	return nil
}

// Disconnect stays connected when it fails, as go-libvirt does: a failed close
// RPC returns before the socket is closed.
func (f *fakeLibvirt) Disconnect() error {
	defer f.mu.Unlock()
	if err := f.enter("Disconnect", ""); err != nil {
		return err
	}
	f.connected = false
	return nil
}

func (f *fakeLibvirt) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isConnectedCalls++
	return f.connected
}

func (f *fakeLibvirt) DomainLookupByName(name string) (golibvirt.Domain, error) {
	defer f.mu.Unlock()
	if err := f.enter("DomainLookupByName", name); err != nil {
		return golibvirt.Domain{}, err
	}
	d, ok := f.domains[name]
	if !ok {
		return golibvirt.Domain{}, lvErr(golibvirt.ErrNoDomain)
	}
	return d.dom, nil
}

func (f *fakeLibvirt) DomainLookupByUUID(uuid golibvirt.UUID) (golibvirt.Domain, error) {
	defer f.mu.Unlock()
	if err := f.enter("DomainLookupByUUID", formatUUID(uuid)); err != nil {
		return golibvirt.Domain{}, err
	}
	for _, d := range f.domains {
		if d.dom.UUID == uuid {
			return d.dom, nil
		}
	}
	return golibvirt.Domain{}, lvErr(golibvirt.ErrNoDomain)
}

func (f *fakeLibvirt) DomainDefineXML(desc string) (golibvirt.Domain, error) {
	var def domainDef
	if err := xml.Unmarshal([]byte(desc), &def); err != nil {
		return golibvirt.Domain{}, lvErr(golibvirt.ErrXMLError)
	}
	defer f.mu.Unlock()
	if err := f.enter("DomainDefineXML", def.Name); err != nil {
		return golibvirt.Domain{}, err
	}
	// domainXML carries no <uuid>, so libvirt generates one and refuses the name
	// as already taken by a domain with another.
	if _, ok := f.domains[def.Name]; ok {
		return golibvirt.Domain{}, lvErr(golibvirt.ErrOperationFailed)
	}
	return f.addDomain(def.Name, desc, false).dom, nil
}

func (f *fakeLibvirt) domain(dom golibvirt.Domain) (*fakeDomain, error) {
	d, ok := f.domains[dom.Name]
	if !ok {
		return nil, lvErr(golibvirt.ErrNoDomain)
	}
	return d, nil
}

func (f *fakeLibvirt) DomainIsActive(dom golibvirt.Domain) (int32, error) {
	defer f.mu.Unlock()
	if err := f.enter("DomainIsActive", dom.Name); err != nil {
		return 0, err
	}
	d, err := f.domain(dom)
	if err != nil {
		return 0, err
	}
	if d.active {
		return 1, nil
	}
	return 0, nil
}

func (f *fakeLibvirt) DomainGetXMLDesc(dom golibvirt.Domain, _ golibvirt.DomainXMLFlags) (string, error) {
	defer f.mu.Unlock()
	if err := f.enter("DomainGetXMLDesc", dom.Name); err != nil {
		return "", err
	}
	d, err := f.domain(dom)
	if err != nil {
		return "", err
	}
	return d.xml, nil
}

func (f *fakeLibvirt) DomainCreate(dom golibvirt.Domain) error {
	defer f.mu.Unlock()
	if err := f.enter("DomainCreate", dom.Name); err != nil {
		return err
	}
	d, err := f.domain(dom)
	if err != nil {
		return err
	}
	d.active = true
	return nil
}

func (f *fakeLibvirt) DomainDestroy(dom golibvirt.Domain) error {
	defer f.mu.Unlock()
	if err := f.enter("DomainDestroy", dom.Name); err != nil {
		return err
	}
	d, err := f.domain(dom)
	if err != nil {
		return err
	}
	if !d.active {
		return lvErr(golibvirt.ErrOperationInvalid)
	}
	d.active = false
	return nil
}

func (f *fakeLibvirt) DomainUndefineFlags(dom golibvirt.Domain, _ golibvirt.DomainUndefineFlagsValues) error {
	defer f.mu.Unlock()
	if err := f.enter("DomainUndefineFlags", dom.Name); err != nil {
		return err
	}
	if _, err := f.domain(dom); err != nil {
		return err
	}
	delete(f.domains, dom.Name)
	return nil
}

func (f *fakeLibvirt) DomainGetState(dom golibvirt.Domain, _ uint32) (int32, int32, error) {
	defer f.mu.Unlock()
	if err := f.enter("DomainGetState", dom.Name); err != nil {
		return 0, 0, err
	}
	d, err := f.domain(dom)
	if err != nil {
		return 0, 0, err
	}
	if d.active {
		return int32(golibvirt.DomainRunning), 0, nil
	}
	return int32(golibvirt.DomainShutoff), 0, nil
}

func (f *fakeLibvirt) DomainInterfaceAddresses(dom golibvirt.Domain, source, _ uint32) ([]golibvirt.DomainInterface, error) {
	defer f.mu.Unlock()
	if err := f.enter("DomainInterfaceAddresses", dom.Name); err != nil {
		return nil, err
	}
	d, err := f.domain(dom)
	if err != nil {
		return nil, err
	}
	ifaces, ok := d.addrs[source]
	if !ok {
		return nil, lvErr(golibvirt.ErrAgentUnresponsive)
	}
	return ifaces, nil
}

// ConnectListAllStoragePools honours the active and inactive filters the way
// libvirt does: either one alone narrows the list, neither or both list all.
func (f *fakeLibvirt) ConnectListAllStoragePools(_ int32, flags golibvirt.ConnectListAllStoragePoolsFlags) ([]golibvirt.StoragePool, uint32, error) {
	defer f.mu.Unlock()
	if err := f.enter("ConnectListAllStoragePools", ""); err != nil {
		return nil, 0, err
	}
	wantActive := flags&golibvirt.ConnectListStoragePoolsActive != 0
	wantInactive := flags&golibvirt.ConnectListStoragePoolsInactive != 0
	names := make([]string, 0, len(f.pools))
	for n, p := range f.pools {
		if wantActive != wantInactive && p.active != wantActive {
			continue
		}
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]golibvirt.StoragePool, 0, len(names))
	for _, n := range names {
		out = append(out, golibvirt.StoragePool{Name: n})
	}
	return out, uint32(len(out)), nil
}

func (f *fakeLibvirt) StoragePoolLookupByName(name string) (golibvirt.StoragePool, error) {
	defer f.mu.Unlock()
	if err := f.enter("StoragePoolLookupByName", name); err != nil {
		return golibvirt.StoragePool{}, err
	}
	if _, ok := f.pools[name]; !ok {
		return golibvirt.StoragePool{}, lvErr(golibvirt.ErrNoStoragePool)
	}
	return golibvirt.StoragePool{Name: name}, nil
}

func (f *fakeLibvirt) pool(name string) (*fakePool, error) {
	p, ok := f.pools[name]
	if !ok {
		return nil, lvErr(golibvirt.ErrNoStoragePool)
	}
	return p, nil
}

// activePool is pool for the volume calls, which libvirt refuses against a
// pool that is not running.
func (f *fakeLibvirt) activePool(name string) (*fakePool, error) {
	p, err := f.pool(name)
	if err != nil {
		return nil, err
	}
	if !p.active {
		return nil, lvErr(golibvirt.ErrOperationInvalid)
	}
	return p, nil
}

func (f *fakeLibvirt) StoragePoolIsActive(pool golibvirt.StoragePool) (int32, error) {
	defer f.mu.Unlock()
	if err := f.enter("StoragePoolIsActive", pool.Name); err != nil {
		return 0, err
	}
	p, err := f.pool(pool.Name)
	if err != nil {
		return 0, err
	}
	if p.active {
		return 1, nil
	}
	return 0, nil
}

// StoragePoolGetXMLDesc answers for a stopped pool too, as libvirt does.
func (f *fakeLibvirt) StoragePoolGetXMLDesc(pool golibvirt.StoragePool, _ golibvirt.StorageXMLFlags) (string, error) {
	defer f.mu.Unlock()
	if err := f.enter("StoragePoolGetXMLDesc", pool.Name); err != nil {
		return "", err
	}
	p, err := f.pool(pool.Name)
	if err != nil {
		return "", err
	}
	if p.xml != "" {
		return p.xml, nil
	}
	return "<pool type='dir'><name>" + p.name + "</name><target><path>" + p.dir + "</path></target></pool>", nil
}

func (f *fakeLibvirt) StorageVolCreateXML(pool golibvirt.StoragePool, desc string, _ golibvirt.StorageVolCreateFlags) (golibvirt.StorageVol, error) {
	var def volumeDef
	if err := xml.Unmarshal([]byte(desc), &def); err != nil {
		return golibvirt.StorageVol{}, lvErr(golibvirt.ErrXMLError)
	}
	defer f.mu.Unlock()
	if err := f.enter("StorageVolCreateXML", def.Name); err != nil {
		return golibvirt.StorageVol{}, err
	}
	p, err := f.activePool(pool.Name)
	if err != nil {
		return golibvirt.StorageVol{}, err
	}
	if _, ok := p.vols[def.Name]; ok {
		return golibvirt.StorageVol{}, lvErr(golibvirt.ErrStorageVolExist)
	}
	p.vols[def.Name] = &fakeVol{xml: desc}
	return golibvirt.StorageVol{Pool: pool.Name, Name: def.Name}, nil
}

func (f *fakeLibvirt) StorageVolLookupByName(pool golibvirt.StoragePool, name string) (golibvirt.StorageVol, error) {
	defer f.mu.Unlock()
	if err := f.enter("StorageVolLookupByName", name); err != nil {
		return golibvirt.StorageVol{}, err
	}
	p, err := f.activePool(pool.Name)
	if err != nil {
		return golibvirt.StorageVol{}, err
	}
	if _, ok := p.vols[name]; !ok {
		return golibvirt.StorageVol{}, lvErr(golibvirt.ErrNoStorageVol)
	}
	return golibvirt.StorageVol{Pool: pool.Name, Name: name}, nil
}

func (f *fakeLibvirt) StorageVolLookupByPath(volPath string) (golibvirt.StorageVol, error) {
	defer f.mu.Unlock()
	if err := f.enter("StorageVolLookupByPath", volPath); err != nil {
		return golibvirt.StorageVol{}, err
	}
	// Only running pools are searched; a volume in a stopped one is not found.
	for _, p := range f.pools {
		if !p.active || path.Dir(volPath) != p.dir {
			continue
		}
		if _, ok := p.vols[path.Base(volPath)]; ok {
			return golibvirt.StorageVol{Pool: p.name, Name: path.Base(volPath)}, nil
		}
	}
	return golibvirt.StorageVol{}, lvErr(golibvirt.ErrNoStorageVol)
}

func (f *fakeLibvirt) StorageVolGetPath(vol golibvirt.StorageVol) (string, error) {
	defer f.mu.Unlock()
	if err := f.enter("StorageVolGetPath", vol.Name); err != nil {
		return "", err
	}
	p, err := f.activePool(vol.Pool)
	if err != nil {
		return "", err
	}
	return p.dir + "/" + vol.Name, nil
}

func (f *fakeLibvirt) StorageVolUpload(vol golibvirt.StorageVol, r io.Reader, _, _ uint64, _ golibvirt.StorageVolUploadFlags) error {
	data, readErr := io.ReadAll(r)
	defer f.mu.Unlock()
	if err := f.enter("StorageVolUpload", vol.Name); err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	p, err := f.activePool(vol.Pool)
	if err != nil {
		return err
	}
	v, ok := p.vols[vol.Name]
	if !ok {
		return lvErr(golibvirt.ErrNoStorageVol)
	}
	v.data = data
	return nil
}

func (f *fakeLibvirt) StorageVolDelete(vol golibvirt.StorageVol, _ golibvirt.StorageVolDeleteFlags) error {
	defer f.mu.Unlock()
	if err := f.enter("StorageVolDelete", vol.Name); err != nil {
		return err
	}
	p, err := f.activePool(vol.Pool)
	if err != nil {
		return err
	}
	if _, ok := p.vols[vol.Name]; !ok {
		return lvErr(golibvirt.ErrNoStorageVol)
	}
	delete(p.vols, vol.Name)
	return nil
}

func (f *fakeLibvirt) NetworkLookupByName(name string) (golibvirt.Network, error) {
	defer f.mu.Unlock()
	if err := f.enter("NetworkLookupByName", name); err != nil {
		return golibvirt.Network{}, err
	}
	n, ok := f.networks[name]
	if !ok {
		return golibvirt.Network{}, lvErr(golibvirt.ErrNoNetwork)
	}
	return n.net, nil
}

// NetworkDefineXML fills in a bridge name when the definition leaves it out,
// as libvirt does.
func (f *fakeLibvirt) NetworkDefineXML(desc string) (golibvirt.Network, error) {
	var def networkDef
	if err := xml.Unmarshal([]byte(desc), &def); err != nil {
		return golibvirt.Network{}, lvErr(golibvirt.ErrXMLError)
	}
	defer f.mu.Unlock()
	if err := f.enter("NetworkDefineXML", def.Name); err != nil {
		return golibvirt.Network{}, err
	}
	if _, ok := f.networks[def.Name]; ok {
		return golibvirt.Network{}, lvErr(golibvirt.ErrOperationFailed)
	}
	if def.Bridge.Name == "" {
		def.Bridge.Name = "virbr-" + def.Name
	}
	return f.addNetwork(def.Name, def, false).net, nil
}

func (f *fakeLibvirt) network(n golibvirt.Network) (*fakeNetwork, error) {
	got, ok := f.networks[n.Name]
	if !ok {
		return nil, lvErr(golibvirt.ErrNoNetwork)
	}
	return got, nil
}

func (f *fakeLibvirt) NetworkIsActive(n golibvirt.Network) (int32, error) {
	defer f.mu.Unlock()
	if err := f.enter("NetworkIsActive", n.Name); err != nil {
		return 0, err
	}
	got, err := f.network(n)
	if err != nil {
		return 0, err
	}
	if got.active {
		return 1, nil
	}
	return 0, nil
}

func (f *fakeLibvirt) NetworkCreate(n golibvirt.Network) error {
	defer f.mu.Unlock()
	if err := f.enter("NetworkCreate", n.Name); err != nil {
		return err
	}
	got, err := f.network(n)
	if err != nil {
		return err
	}
	// "network is already active", not ErrNetworkExist.
	if got.active {
		return lvErr(golibvirt.ErrOperationInvalid)
	}
	got.active = true
	return nil
}

func (f *fakeLibvirt) NetworkSetAutostart(n golibvirt.Network, autostart int32) error {
	defer f.mu.Unlock()
	if err := f.enter("NetworkSetAutostart", n.Name); err != nil {
		return err
	}
	got, err := f.network(n)
	if err != nil {
		return err
	}
	got.autostart = autostart == 1
	return nil
}

func (f *fakeLibvirt) NetworkGetXMLDesc(n golibvirt.Network, _ uint32) (string, error) {
	defer f.mu.Unlock()
	if err := f.enter("NetworkGetXMLDesc", n.Name); err != nil {
		return "", err
	}
	got, err := f.network(n)
	if err != nil {
		return "", err
	}
	return got.xml, nil
}
