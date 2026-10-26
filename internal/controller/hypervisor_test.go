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

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers/fake"
)

const (
	hv2Written   = "hv2.lab.test"       // as an operator writes it
	hv2Address   = "hv2.lab.test:16514" // as it is recorded and dialled
	hv2Secret    = "wl0-libvirt-pki"
	hv3Address   = "hv3.lab.test:16514"
	hvIPv6       = "[fd00::13]:16514"
	worker2      = "worker-2"
	testDataPEM  = "#cloud-config\n"
	waitingReady = "WaitingForHypervisor"
)

var (
	testPKIOnce sync.Once
	testCertPEM []byte
	testKeyPEM  []byte
)

// testPKI is a self-signed certificate and its key, standing in for both the
// CA and the client identity: enough for the credentials to parse, which is all
// the controller checks.
func testPKI() (certPEM, keyPEM []byte) {
	testPKIOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: "hydra-test"},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			IsCA:                  true,
			BasicConstraintsValid: true,
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		Expect(err).NotTo(HaveOccurred())
		keyDER, err := x509.MarshalECPrivateKey(key)
		Expect(err).NotTo(HaveOccurred())
		testCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		testKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	})
	return testCertPEM, testKeyPEM
}

// credentialsSecret is hv2's credentials Secret as an operator creates it.
func credentialsSecret() *corev1.Secret {
	cert, key := testPKI()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: hv2Secret, Namespace: linkNamespace, UID: types.UID("uid-" + hv2Secret)},
		Data: map[string][]byte{
			hypervisorCACertKey:     cert,
			hypervisorClientCertKey: cert,
			hypervisorClientKeyKey:  key,
		},
	}
}

// namingHV2 makes a HydraCluster name hv2, unconfirmed.
func namingHV2(hc *infrav1.HydraCluster) {
	hc.Spec.Hypervisor = &infrav1.HydraHypervisor{
		Address:              hv2Written,
		CredentialsSecretRef: infrav1.HydraSecretReference{Name: hv2Secret},
	}
}

// confirmedHV2 makes a HydraCluster name hv2, confirmed.
func confirmedHV2(hc *infrav1.HydraCluster) {
	namingHV2(hc)
	hc.Status.Hypervisor = &infrav1.HydraHypervisorStatus{Address: hv2Written}
}

// hypervisors is a ProviderFor over two fake hypervisors: the manager's flag
// connection and hv2. It records every request, and every release.
type hypervisors struct {
	flags, hv2 *fake.Provider
	asked      []*providers.Hypervisor
	owners     []providers.Owner
	released   []providers.Owner
}

func newHypervisors() *hypervisors {
	return &hypervisors{flags: fake.New(), hv2: fake.New()}
}

func (h *hypervisors) providerFor(_ context.Context, owner providers.Owner, hv *providers.Hypervisor) (providers.MachineProvider, error) {
	h.asked = append(h.asked, hv)
	h.owners = append(h.owners, owner)
	switch {
	case hv == nil:
		return h.flags, nil
	case hv.Address == hv2Address:
		return h.hv2, nil
	default:
		return nil, errors.New("no such hypervisor: " + hv.Address)
	}
}

func (h *hypervisors) release(_ context.Context, owner providers.Owner) {
	h.released = append(h.released, owner)
}

// touched reports whether anything at all was asked of p.
func touched(p *fake.Provider) bool {
	return p.CreateCalls > 0 || p.DeleteCalls > 0 || len(p.DeleteByNamePools) > 0 || p.InfrastructureChecks > 0
}

var _ = Describe("Hypervisor addresses", func() {
	It("fills in libvirt's TLS port and keeps IPv6 brackets", func() {
		for in, want := range map[string]string{
			hv2Written:        hv2Address,
			hv2Address:        hv2Address,
			"192.168.15.13:1": "192.168.15.13:1",
			"[fd00::13]":      hvIPv6,
			hvIPv6:            hvIPv6,
		} {
			got, err := canonicalHypervisor(in)
			Expect(err).NotTo(HaveOccurred(), in)
			Expect(got).To(Equal(want), in)
		}
	})

	It("refuses a port out of range terminally, since only the spec can fix it", func() {
		for _, in := range []string{"hv2.lab.test:0", "hv2.lab.test:65536", "hv2.lab.test:99999"} {
			_, err := canonicalHypervisor(in)
			Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue(), in)
		}
	})
})

var _ = Describe("Summaries in condition messages", func() {
	It("names a few and counts the rest", func() {
		Expect(summarize([]string{"a", "b"}, ", ")).To(Equal("a, b"))
		many := make([]string, 0, 1000)
		for i := range 1000 {
			many = append(many, fmt.Sprintf("worker-%04d has no domain named default-worker-%04d there", i, i))
		}
		out := summarize(many, "; ")
		Expect(out).To(HaveSuffix("; and 995 more, 1000 in all"))
		Expect(strings.Count(out, "worker-")).To(Equal(2 * maxListed))
	})
})

var _ = Describe("Hypervisor credentials", func() {
	ctx := context.Background()

	read := func(secret *corev1.Secret) (*providers.Hypervisor, error) {
		hc := hydraCluster(namingHV2)
		b := fakeclient.NewClientBuilder().WithScheme(linkScheme())
		if secret != nil {
			b = b.WithObjects(secret)
		}
		return hypervisorOf(ctx, b.Build(), hc)
	}

	It("reads the Secret into a connection request, versioned by UID and resourceVersion", func() {
		hv, err := read(credentialsSecret())
		Expect(err).NotTo(HaveOccurred())
		Expect(hv.Address).To(Equal(hv2Address))
		cert, key := testPKI()
		Expect(hv.Credentials.ClientCert).To(Equal(cert))
		Expect(hv.Credentials.ClientKey).To(Equal(key))
		Expect(hv.CredentialsVersion).To(HavePrefix("uid-" + hv2Secret + "/"))
	})

	It("is terminal for a missing Secret, a missing key, or credentials that do not parse", func() {
		_, err := read(nil)
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("does not exist"))

		missing := credentialsSecret()
		delete(missing.Data, hypervisorClientKeyKey)
		_, err = read(missing)
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring(hypervisorClientKeyKey))

		mismatched := credentialsSecret()
		mismatched.Data[hypervisorClientKeyKey] = []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n")
		_, err = read(mismatched)
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue())

		noCA := credentialsSecret()
		noCA.Data[hypervisorCACertKey] = []byte("not a certificate")
		_, err = read(noCA)
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring(hypervisorCACertKey))
	})

	It("never prints the key", func() {
		hv, err := read(credentialsSecret())
		Expect(err).NotTo(HaveOccurred())
		_, key := testPKI()
		for _, out := range []string{
			fmtAll("%v", hv), fmtAll("%+v", hv), fmtAll("%#v", *hv), fmtAll("%d", *hv),
		} {
			Expect(out).NotTo(ContainSubstring(string(key[30:60])))
		}
	})
})

var _ = Describe("Machines on a named hypervisor", func() {
	ctx := context.Background()

	var (
		hvs *hypervisors
		hm  *infrav1.HydraMachine
		key types.NamespacedName
	)

	build := func(hcMutate func(*infrav1.HydraCluster), objs ...client.Object) *HydraMachineReconciler {
		s := linkScheme()
		secretName := linkSecretName
		all := append([]client.Object{
			hm,
			ownerMachine(&secretName),
			bootstrapSecret(secretName, map[string][]byte{bootstrapDataSecretKey: []byte(testDataPEM)}),
			owningCluster(nil),
			credentialsSecret(),
		}, objs...)
		if hcMutate != nil {
			all = append(all, verifiedHydraCluster(hcMutate))
		}
		c := fakeclient.NewClientBuilder().
			WithScheme(s).
			WithObjects(all...).
			WithStatusSubresource(&infrav1.HydraMachine{}).
			Build()
		return &HydraMachineReconciler{
			Client: c, APIReader: c, Scheme: s,
			ProviderFor: hvs.providerFor, ReleaseProvider: hvs.release,
		}
	}

	reload := func(r *HydraMachineReconciler) *infrav1.HydraMachine {
		out := &infrav1.HydraMachine{}
		Expect(r.Get(ctx, key, out)).To(Succeed())
		return out
	}

	ready := func(r *HydraMachineReconciler) *metav1.Condition {
		return apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.MachineReadyCondition)
	}

	BeforeEach(func() {
		hvs = newHypervisors()
		hm = linkedMachine(linkMachineName)
		key = client.ObjectKeyFromObject(hm)
	})

	It("creates a new machine on its cluster's confirmed hypervisor, owned by the machine", func() {
		r := build(confirmedHV2)
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(hvs.hv2.CreateCalls).To(Equal(1))
		Expect(touched(hvs.flags)).To(BeFalse())
		Expect(reload(r).Annotations).To(HaveKeyWithValue(infrav1.HypervisorAnnotation, hv2Address))
		Expect(hvs.owners).To(ContainElement(machineOwner(key)))
		Expect(hvs.asked[len(hvs.asked)-1].Credentials).NotTo(BeNil())
	})

	It("records the hypervisor before Create runs", func() {
		// A Create that fails after allocating volumes leaves exactly what
		// teardown has to find, and it can only look on the right hypervisor if
		// that was recorded already.
		hvs.hv2.CreateErr = errors.New("crashed after allocating volumes")
		r := build(confirmedHV2)
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).To(HaveOccurred())
		Expect(reload(r).Annotations).To(HaveKeyWithValue(infrav1.HypervisorAnnotation, hv2Address))
	})

	It("waits, creating nothing anywhere, while the cluster's hypervisor is unconfirmed", func() {
		r := build(namingHV2)
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(requeueWhileProvisioning))
		Expect(ready(r).Reason).To(Equal(waitingReady))
		Expect(touched(hvs.flags) || touched(hvs.hv2)).To(BeFalse())
		Expect(reload(r).Annotations).NotTo(HaveKey(infrav1.HypervisorAnnotation))
	})

	It("keeps a machine made on the flag connection there, whatever the cluster names now", func() {
		// It has a providerID and records no hypervisor, so it was made before
		// its cluster named one. Until the HydraCluster controller has found it
		// on hv2, the flag connection is where it is.
		created, err := hvs.flags.Create(ctx, providers.MachineSpec{Name: backendName(hm)})
		Expect(err).NotTo(HaveOccurred())
		providerID := providers.ProviderID("fake", created.ID)
		hm.Spec.ProviderID = &providerID
		hm.Finalizers = []string{MachineFinalizer}
		r := build(namingHV2)

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.asked).To(ConsistOf(BeNil()))
		Expect(touched(hvs.hv2)).To(BeFalse())
	})

	It("tears a recorded machine down on its hypervisor and releases its connection", func() {
		created, err := hvs.hv2.Create(ctx, providers.MachineSpec{Name: backendName(hm)})
		Expect(err).NotTo(HaveOccurred())
		providerID := providers.ProviderID("fake", created.ID)
		hm.Spec.ProviderID = &providerID
		hm.Annotations = map[string]string{infrav1.HypervisorAnnotation: hv2Address}
		hm.Finalizers = []string{MachineFinalizer}
		r := build(confirmedHV2)
		Expect(r.Delete(ctx, hm)).To(Succeed())

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.hv2.DeleteCalls).To(BeNumerically(">=", 1)) // the ID, then the by-name sweep
		Expect(hvs.hv2.Count()).To(Equal(0))
		Expect(touched(hvs.flags)).To(BeFalse())
		Expect(hvs.released).To(ContainElement(machineOwner(key)))
	})

	It("never falls back to the flag connection for a recorded machine whose cluster no longer names its hypervisor", func() {
		// The flag connection may be another hypervisor, where a domain of the
		// same name could be someone else's. Teardown there would destroy it.
		for name, mutate := range map[string]func(*infrav1.HydraCluster){
			"names another": func(hc *infrav1.HydraCluster) {
				confirmedHV2(hc)
				hc.Spec.Hypervisor.Address = hv3Address
				hc.Status.Hypervisor.Address = hv3Address
			},
			"names none": func(*infrav1.HydraCluster) {},
			"is gone":    nil,
		} {
			hvs = newHypervisors()
			hm = linkedMachine(linkMachineName)
			providerID := providers.ProviderID("fake", "fake-1")
			hm.Spec.ProviderID = &providerID
			hm.Annotations = map[string]string{infrav1.HypervisorAnnotation: hv2Address}
			hm.Finalizers = []string{MachineFinalizer}
			r := build(mutate)
			Expect(r.Delete(ctx, hm)).To(Succeed())

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred(), name)
			Expect(res.RequeueAfter).To(Equal(requeueWhileProvisioning), name)
			Expect(ready(r).Reason).To(Equal("WaitingForHypervisorCredentials"), name)
			Expect(hvs.asked).To(BeEmpty(), name)
			Expect(reload(r).Finalizers).To(ContainElement(MachineFinalizer), name)
		}
	})

	It("waits, rather than failing, while its credentials Secret is missing or broken", func() {
		// The Secret is the HydraCluster's problem, reported terminally there.
		// ProvisioningFailed on every machine of the cluster, running ones
		// included, would invite remediation of machines that are fine -- in
		// the middle of an operator replacing the Secret, say.
		hm.Annotations = map[string]string{infrav1.HypervisorAnnotation: hv2Address}
		r := build(func(hc *infrav1.HydraCluster) {
			confirmedHV2(hc)
			hc.Spec.Hypervisor.CredentialsSecretRef.Name = "not-there"
		})
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(requeueWhileProvisioning))
		Expect(ready(r).Reason).To(Equal("WaitingForHypervisorCredentials"))
		Expect(ready(r).Message).To(ContainSubstring(`"not-there" does not exist`))
		Expect(apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.MachineProvisioningFailedCondition)).To(BeNil())
		Expect(hvs.asked).To(BeEmpty())
	})

	It("keeps a machine whose Create began on the flag connection there, though it has no providerID", func() {
		// The storage pool is recorded just before Create, so a machine that
		// records one may have left a domain or volumes on the flag connection
		// -- a crash before its providerID was recorded. Sending it to hv2 would
		// make a second VM there and orphan the first.
		hm.Annotations = map[string]string{infrav1.StoragePoolAnnotation: linkPool}
		created, err := hvs.flags.Create(ctx, providers.MachineSpec{Name: backendName(hm)})
		Expect(err).NotTo(HaveOccurred())
		r := build(confirmedHV2)

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(touched(hvs.hv2)).To(BeFalse())
		Expect(hvs.hv2.Count()).To(BeZero())
		Expect(*reload(r).Spec.ProviderID).To(Equal(providers.ProviderID("fake", created.ID)), "adopted where it was made")
		Expect(reload(r).Annotations).NotTo(HaveKey(infrav1.HypervisorAnnotation))
	})

	It("records the hypervisor when it adopts a domain it finds there, so it stays there", func() {
		// A domain already carries the machine's name on hv2 -- an earlier
		// attempt that recorded nothing. Adopting it without recording hv2
		// would leave a providerID and no record, which means the flags.
		found, err := hvs.hv2.Create(ctx, providers.MachineSpec{Name: backendName(hm)})
		Expect(err).NotTo(HaveOccurred())
		r := build(confirmedHV2)

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		adopted := reload(r)
		Expect(*adopted.Spec.ProviderID).To(Equal(providers.ProviderID("fake", found.ID)))
		Expect(adopted.Annotations).To(HaveKeyWithValue(infrav1.HypervisorAnnotation, hv2Address))
		Expect(adopted.Annotations).NotTo(HaveKey(infrav1.StoragePoolAnnotation),
			"adoption takes the domain as it is; it resolves no pool")

		hvs.asked = nil
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.asked).To(HaveLen(1))
		Expect(hvs.asked[0]).NotTo(BeNil(), "still hv2, not the flags")
		Expect(touched(hvs.flags)).To(BeFalse())
	})

	It("tears a machine nothing was ever attempted for down on the confirmed hypervisor, not the flags", func() {
		// Deleted while waiting for its bootstrap data: it has nothing anywhere.
		// Looking on the flag connection would make its deletion depend on a
		// hypervisor this cluster no longer uses.
		hm.Finalizers = []string{MachineFinalizer}
		r := build(confirmedHV2)
		Expect(r.Delete(ctx, hm)).To(Succeed())

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.hv2.DeleteByNamePools).To(HaveLen(1))
		Expect(touched(hvs.flags)).To(BeFalse())
	})

	It("tears down an ownerless machine nothing was attempted for on its cluster's hypervisor, found by label", func() {
		// Deleted before its first Create, with its owner Machine gone too. The
		// cluster has left the flags, which may be unreachable; the deletion
		// must not wait on a hypervisor the machine never used.
		hm.Finalizers = []string{MachineFinalizer}
		r := build(confirmedHV2)
		secretName := linkSecretName
		Expect(r.Delete(ctx, ownerMachine(&secretName))).To(Succeed())
		Expect(r.Delete(ctx, hm)).To(Succeed())

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.hv2.DeleteByNamePools).To(Equal([]string{linkPool}), "the cluster's pool, found by label")
		Expect(touched(hvs.flags)).To(BeFalse())
	})

	It("keeps an ownerless machine's teardown paused when the Cluster it finds by label is paused", func() {
		// Pausing covers deletion. A Cluster found through the machine's label
		// rather than its owner Machine is still the Cluster, and its pause
		// still holds.
		created, err := hvs.hv2.Create(ctx, providers.MachineSpec{Name: backendName(hm)})
		Expect(err).NotTo(HaveOccurred())
		providerID := providers.ProviderID("fake", created.ID)
		hm.Spec.ProviderID = &providerID
		hm.Annotations = map[string]string{infrav1.HypervisorAnnotation: hv2Address}
		hm.Finalizers = []string{MachineFinalizer}
		r := build(confirmedHV2)
		cluster := owningCluster(nil)
		Expect(r.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		paused := true
		cluster.Spec.Paused = &paused
		Expect(r.Update(ctx, cluster)).To(Succeed())
		secretName := linkSecretName
		Expect(r.Delete(ctx, ownerMachine(&secretName))).To(Succeed())
		Expect(r.Delete(ctx, hm)).To(Succeed())

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.hv2.Count()).To(Equal(1), "the VM is still there")
		Expect(hvs.hv2.DeleteCalls).To(BeZero())
		out := reload(r)
		Expect(out.Finalizers).To(ContainElement(MachineFinalizer))
		pausedCond := apimeta.FindStatusCondition(out.Status.Conditions, infrav1.MachinePausedCondition)
		Expect(pausedCond).NotTo(BeNil())
		Expect(pausedCond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("finds its credentials through its cluster-name label when its owner Machine is gone", func() {
		// The owner Machine's finalizer removed by hand, say. The HydraCluster is
		// still there; teardown must not wedge over how it was found.
		created, err := hvs.hv2.Create(ctx, providers.MachineSpec{Name: backendName(hm)})
		Expect(err).NotTo(HaveOccurred())
		providerID := providers.ProviderID("fake", created.ID)
		hm.Spec.ProviderID = &providerID
		hm.Annotations = map[string]string{infrav1.HypervisorAnnotation: hv2Address}
		hm.Finalizers = []string{MachineFinalizer}
		r := build(confirmedHV2)
		secretName := linkSecretName
		Expect(r.Delete(ctx, ownerMachine(&secretName))).To(Succeed())
		Expect(r.Delete(ctx, hm)).To(Succeed())

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.hv2.Count()).To(BeZero())
		Expect(touched(hvs.flags)).To(BeFalse())
	})

	It("releases a machine that is gone", func() {
		r := build(confirmedHV2)
		Expect(r.Delete(ctx, hm)).To(Succeed())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.released).To(Equal([]providers.Owner{machineOwner(key)}))
	})
})

var _ = Describe("Confirming a cluster's hypervisor", func() {
	ctx := context.Background()

	var (
		hvs *hypervisors
		hc  *infrav1.HydraCluster
		key types.NamespacedName
	)

	buildWith := func(funcs *interceptor.Funcs, objs ...client.Object) *HydraClusterReconciler {
		s := linkScheme()
		all := append([]client.Object{hc, credentialsSecret()}, objs...)
		b := fakeclient.NewClientBuilder().
			WithScheme(s).
			WithObjects(all...).
			WithStatusSubresource(&infrav1.HydraCluster{}, &infrav1.HydraMachine{})
		if funcs != nil {
			b = b.WithInterceptorFuncs(*funcs)
		}
		c := b.Build()
		return &HydraClusterReconciler{
			Client: c, APIReader: c, Scheme: s,
			ProviderFor: hvs.providerFor, ReleaseProvider: hvs.release,
		}
	}

	build := func(objs ...client.Object) *HydraClusterReconciler { return buildWith(nil, objs...) }

	reload := func(r *HydraClusterReconciler) *infrav1.HydraCluster {
		out := &infrav1.HydraCluster{}
		Expect(r.Get(ctx, key, out)).To(Succeed())
		return out
	}

	machineIn := func(r *HydraClusterReconciler, name string) *infrav1.HydraMachine {
		out := &infrav1.HydraMachine{}
		Expect(r.Get(ctx, types.NamespacedName{Namespace: linkNamespace, Name: name}, out)).To(Succeed())
		return out
	}

	// made is a machine of the cluster that was created on p, with the ID p gave
	// it, recording no hypervisor: a machine from before the cluster named one.
	made := func(name string, p *fake.Provider) *infrav1.HydraMachine {
		m := linkedMachine(name)
		created, err := p.Create(ctx, providers.MachineSpec{Name: backendName(m)})
		Expect(err).NotTo(HaveOccurred())
		providerID := providers.ProviderID("fake", created.ID)
		m.Spec.ProviderID = &providerID
		return m
	}

	expectRefused := func(r *HydraClusterReconciler, why string) {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(requeueClusterHealthy))
		out := reload(r)
		Expect(out.Status.Hypervisor).To(BeNil(), "nothing is confirmed")
		failed := apimeta.FindStatusCondition(out.Status.Conditions, infrav1.ClusterInfrastructureFailedCondition)
		Expect(failed).NotTo(BeNil())
		Expect(failed.Message).To(ContainSubstring(why))
	}

	BeforeEach(func() {
		hvs = newHypervisors()
		hc = hydraCluster(namingHV2)
		key = client.ObjectKeyFromObject(hc)
	})

	It("confirms a hypervisor for a cluster with no machines, and checks its infrastructure there", func() {
		r := build(owningCluster(nil))
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Hypervisor).To(Equal(&infrav1.HydraHypervisorStatus{Address: hv2Written}))
		Expect(hvs.hv2.InfrastructureChecks).To(Equal(1))
		Expect(touched(hvs.flags)).To(BeFalse())
		Expect(hvs.owners).To(ContainElement(providers.Owner{Kind: hydraClusterKind, NamespacedName: key}))
	})

	It("confirms once every machine is found there by name and ID, then records it on each", func() {
		// The lab's case: the flag connection already reaches hv2, so the
		// machines made through it are on hv2.
		w1 := made("worker-1", hvs.hv2)
		w2 := made(worker2, hvs.hv2)
		r := build(owningCluster(nil), w1, w2)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Hypervisor).NotTo(BeNil())
		for _, name := range []string{"worker-1", worker2} {
			Expect(machineIn(r, name).Annotations).To(HaveKeyWithValue(infrav1.HypervisorAnnotation, hv2Address), name)
		}
	})

	It("refuses, recording nothing, when a machine is not there", func() {
		on := made("worker-1", hvs.hv2)
		off := made(worker2, hvs.flags)
		r := build(owningCluster(nil), on, off)
		expectRefused(r, "worker-2 has no domain named")
		Expect(machineIn(r, "worker-1").Annotations).NotTo(HaveKey(infrav1.HypervisorAnnotation),
			"read-only until every machine was checked")
	})

	It("refuses a domain there that only shares the machine's name", func() {
		m := made("worker-1", hvs.flags)
		_, err := hvs.hv2.Create(ctx, providers.MachineSpec{Name: "decoy"})
		Expect(err).NotTo(HaveOccurred())
		_, err = hvs.hv2.Create(ctx, providers.MachineSpec{Name: backendName(m)}) // fake-2 on hv2, fake-1 recorded
		Expect(err).NotTo(HaveOccurred())
		r := build(owningCluster(nil), m)
		expectRefused(r, "not the recorded")
	})

	It("refuses when a machine records another hypervisor", func() {
		m := made("worker-1", hvs.hv2)
		m.Annotations = map[string]string{infrav1.HypervisorAnnotation: hv3Address}
		r := build(owningCluster(nil), m)
		expectRefused(r, "records hypervisor "+hv3Address)
	})

	It("skips machines not made yet, and machines being deleted", func() {
		unmade := linkedMachine("worker-1")
		leaving := made(worker2, hvs.flags)
		leaving.Finalizers = []string{MachineFinalizer}
		now := metav1.Now()
		leaving.DeletionTimestamp = &now
		r := build(owningCluster(nil), unmade, leaving)

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Hypervisor).NotTo(BeNil())
		Expect(machineIn(r, "worker-1").Annotations).NotTo(HaveKey(infrav1.HypervisorAnnotation),
			"it records its hypervisor itself, before Create")
		Expect(machineIn(r, worker2).Annotations).NotTo(HaveKey(infrav1.HypervisorAnnotation),
			"its teardown keeps going where it was made")
	})

	It("ignores machines of other clusters", func() {
		other := made("worker-9", hvs.flags)
		other.Labels = map[string]string{"cluster.x-k8s.io/cluster-name": "another"}
		r := build(owningCluster(nil), other)
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Hypervisor).NotTo(BeNil())
	})

	It("waits, without confirming, for a machine whose Create on the flag connection has begun", func() {
		// It records a storage pool and has no providerID yet. Confirming now
		// would let it come back as a machine on the flags with no record -- a
		// cluster then stuck refusing a hypervisor it can no longer change.
		begun := linkedMachine("worker-1")
		begun.Annotations = map[string]string{infrav1.StoragePoolAnnotation: linkPool}
		r := build(owningCluster(nil), begun)
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(requeueClusterUnverified))
		out := reload(r)
		Expect(out.Status.Hypervisor).To(BeNil())
		Expect(apimeta.FindStatusCondition(out.Status.Conditions, infrav1.ClusterInfrastructureFailedCondition)).To(BeNil(),
			"a wait, not something an operator must fix")
		// Still Ready: the machine it waits for needs a Ready cluster to be
		// created at all. Holding Ready back would leave each waiting on the
		// other for ever.
		ready := apimeta.FindStatusCondition(out.Status.Conditions, infrav1.ClusterReadyCondition)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Message).To(ContainSubstring("worker-1"))
	})

	It("lets a machine whose Create on the flags failed early finish there, then confirms (the lab's migration)", func() {
		// The flag connection already reaches hv2, as in the lab. A Create there
		// failed before defining anything, leaving only the pool record. The
		// guard waits for that machine; the machine needs a Ready cluster to
		// retry. Both reconcilers run against the same objects, so a deadlock
		// between them shows here as a machine that is never created.
		hvs.flags = hvs.hv2
		begun := linkedMachine(linkMachineName)
		begun.Annotations = map[string]string{infrav1.StoragePoolAnnotation: linkPool}
		hc.Generation = 1
		secretName := linkSecretName
		clusters := build(owningCluster(nil), begun, ownerMachine(&secretName),
			bootstrapSecret(secretName, map[string][]byte{bootstrapDataSecretKey: []byte(testDataPEM)}))
		machines := &HydraMachineReconciler{
			Client: clusters.Client, APIReader: clusters.Client, Scheme: clusters.Scheme,
			ProviderFor: hvs.providerFor, ReleaseProvider: hvs.release,
		}
		machineKey := client.ObjectKeyFromObject(begun)

		_, err := clusters.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(clusters).Status.Hypervisor).To(BeNil(), "waits for the machine")

		_, err = machines.Reconcile(ctx, ctrl.Request{NamespacedName: machineKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.hv2.CreateCalls).To(Equal(1), "the machine is created where it began")

		_, err = clusters.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(clusters).Status.Hypervisor).NotTo(BeNil())
		Expect(machineIn(clusters, linkMachineName).Annotations).To(HaveKeyWithValue(infrav1.HypervisorAnnotation, hv2Address))
	})

	It("does not confirm a hypervisor that lacks the cluster's infrastructure, so the address can still be corrected", func() {
		// With no machines there is nothing else to check an address against,
		// and confirming fixes it. A host that answers but has none of this
		// cluster's pool or image is the likeliest typo.
		hvs.hv2.InfrastructureErr = fmt.Errorf("%w: storage pool %q does not exist", providers.ErrTerminal, linkPool)
		r := build(owningCluster(nil))
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Hypervisor).To(BeNil())

		hvs.hv2.InfrastructureErr = nil
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Hypervisor).NotTo(BeNil())
	})

	It("retries a conflicting machine patch soon, recording the rest and keeping the cluster Ready", func() {
		// The machine reconciler rewrites a provisioning machine's status every
		// 15s, so a stale cached copy is ordinary. It must not report the
		// cluster unready -- that stops every new machine -- nor hold up the
		// other machines.
		w1 := made("worker-1", hvs.hv2)
		w2 := made(worker2, hvs.hv2)
		var once sync.Once
		r := buildWith(&interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				conflict := false
				if obj.GetName() == "worker-1" {
					once.Do(func() { conflict = true })
				}
				if conflict {
					return apierrors.NewConflict(schema.GroupResource{Resource: "hydramachines"}, obj.GetName(), errors.New("stale"))
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}, owningCluster(nil), w1, w2)

		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(requeueAfterConflict))
		out := reload(r)
		Expect(out.Status.Hypervisor).NotTo(BeNil())
		Expect(apimeta.FindStatusCondition(out.Status.Conditions, infrav1.ClusterReadyCondition).Status).To(Equal(metav1.ConditionTrue))
		Expect(machineIn(r, worker2).Annotations).To(HaveKeyWithValue(infrav1.HypervisorAnnotation, hv2Address))
		Expect(machineIn(r, "worker-1").Annotations).NotTo(HaveKey(infrav1.HypervisorAnnotation))

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(machineIn(r, "worker-1").Annotations).To(HaveKeyWithValue(infrav1.HypervisorAnnotation, hv2Address))
	})

	It("retries a conflicting confirmation soon, confirming nothing and keeping the cluster Ready", func() {
		var once sync.Once
		r := buildWith(&interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				conflict := false
				if hc, ok := obj.(*infrav1.HydraCluster); ok && hc.Status.Hypervisor != nil {
					once.Do(func() { conflict = true })
				}
				if conflict {
					return apierrors.NewConflict(schema.GroupResource{Resource: "hydraclusters"}, obj.GetName(), errors.New("stale"))
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}, owningCluster(nil))

		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(requeueAfterConflict))
		out := reload(r)
		Expect(out.Status.Hypervisor).To(BeNil(), "a lost race confirms nothing")
		Expect(apimeta.FindStatusCondition(out.Status.Conditions, infrav1.ClusterReadyCondition).Status).To(Equal(metav1.ConditionTrue))

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Hypervisor).NotTo(BeNil())
	})

	It("reports a refusal even when another machine is still being created, whichever comes first", func() {
		// A pending machine must not hide a machine that is elsewhere: the
		// refusal is terminal, and it is what stops new machines.
		for _, order := range [][2]string{{linkMachineName, worker2}, {worker2, linkMachineName}} {
			hvs = newHypervisors()
			hc = hydraCluster(namingHV2)
			begun := linkedMachine(order[0])
			begun.Annotations = map[string]string{infrav1.StoragePoolAnnotation: linkPool}
			elsewhere := made(order[1], hvs.flags)
			r := build(owningCluster(nil), begun, elsewhere)
			expectRefused(r, order[1]+" has no domain named")
		}
	})

	It("keeps a refusal about a large cluster short enough to record", func() {
		// A condition message over 32768 characters is refused by the API
		// server, and then nothing says what is wrong.
		objs := make([]client.Object, 0, 13)
		objs = append(objs, owningCluster(nil))
		for i := range 12 {
			objs = append(objs, made(fmt.Sprintf("worker-%02d", i), hvs.flags))
		}
		r := build(objs...)
		expectRefused(r, "and 7 more, 12 in all")
		failed := apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.ClusterInfrastructureFailedCondition)
		Expect(len(failed.Message)).To(BeNumerically("<", 4096))
	})

	It("waits, without confirming, until the cluster has an owning Cluster", func() {
		hc.OwnerReferences = nil
		r := build()
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(requeueClusterUnverified))
		Expect(reload(r).Status.Hypervisor).To(BeNil())
	})

	It("records a machine that gained its providerID after confirmation", func() {
		hc.Status.Hypervisor = &infrav1.HydraHypervisorStatus{Address: hv2Written}
		late := made("worker-1", hvs.hv2)
		r := build(owningCluster(nil), late)
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(machineIn(r, "worker-1").Annotations).To(HaveKeyWithValue(infrav1.HypervisorAnnotation, hv2Address))
	})

	It("lets go of its connection when the hypervisor is removed before confirmation", func() {
		hc.Spec.Hypervisor = nil
		r := build(owningCluster(nil))
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(hvs.released).To(ContainElement(providers.Owner{Kind: hydraClusterKind, NamespacedName: key}))
		Expect(hvs.flags.InfrastructureChecks).To(Equal(1))
	})
})

func fmtAll(format string, v any) string { return fmt.Sprintf(format, v) }
