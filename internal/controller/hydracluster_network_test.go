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
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers/fake"
)

const (
	// testNetworkOwnerID is the network-owner ID of the cluster under test.
	testNetworkOwnerID = "5d6e7f80-1111-4222-8333-444455556666"
	// otherClusterName names a second HydraCluster declaring the same network.
	otherClusterName = "other"
)

// PET-41: a HydraCluster releases the managed network it created when it is
// deleted, and nothing else.
var _ = Describe("HydraCluster Reconciler — releasing a managed network", func() {
	ctx := context.Background()

	var (
		provider *fake.Provider
		hc       *infrav1.HydraCluster
		key      types.NamespacedName
		c        client.Client
		r        *HydraClusterReconciler
	)

	declaring := func(c *infrav1.HydraCluster) {
		c.Spec.ControlPlaneEndpoint.Host = testStatedHost
		c.Spec.ManagedNetwork = &infrav1.HydraManagedNetwork{
			Name: testNetName, Subnet: testSubnet, DHCPStart: testDHCPStart, DHCPEnd: testDHCPEnd,
		}
	}

	build := func(objs ...client.Object) {
		s := linkScheme()
		c = fakeclient.NewClientBuilder().
			WithScheme(s).
			WithObjects(append([]client.Object{hc}, objs...)...).
			WithStatusSubresource(&infrav1.HydraCluster{}).
			Build()
		r = &HydraClusterReconciler{Client: c, Scheme: s, Provider: provider}
	}

	reconcile := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	// deleted marks the cluster for deletion and reconciles once.
	deleted := func() (ctrl.Result, error) {
		Expect(c.Delete(ctx, hc)).To(Succeed())
		return reconcile()
	}

	gone := func() bool {
		return apierrors.IsNotFound(c.Get(ctx, key, &infrav1.HydraCluster{}))
	}

	// ownedAndCreated is a cluster that has reconciled once: finalizer and owner
	// ID recorded, and the backend holding the network it defined for them.
	ownedAndCreated := func(objs ...client.Object) {
		hc = hydraCluster(declaring)
		hc.Finalizers = []string{ClusterFinalizer}
		hc.Annotations = map[string]string{infrav1.NetworkOwnerAnnotation: testNetworkOwnerID}
		key = client.ObjectKeyFromObject(hc)
		build(objs...)
		provider.AddNetwork(testNetName, testNetworkOwnerID)
	}

	BeforeEach(func() {
		provider = fake.New()
	})

	It("records its finalizer and owner ID before the backend can define the network", func() {
		hc = hydraCluster(declaring)
		key = client.ObjectKeyFromObject(hc)
		build(owningCluster(nil))

		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())

		out := &infrav1.HydraCluster{}
		Expect(c.Get(ctx, key, out)).To(Succeed())
		Expect(out.Finalizers).To(ContainElement(ClusterFinalizer))
		id := out.Annotations[infrav1.NetworkOwnerAnnotation]
		Expect(id).NotTo(BeEmpty())
		recorded, ok := provider.HasNetwork(testNetName)
		Expect(ok).To(BeTrue())
		Expect(recorded).To(Equal(id), "the network is defined under the recorded owner")

		// And keeps it: a second pass writes no new ID.
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Get(ctx, key, out)).To(Succeed())
		Expect(out.Annotations[infrav1.NetworkOwnerAnnotation]).To(Equal(id))
	})

	It("adds no finalizer to a cluster without a managed network", func() {
		hc = hydraCluster(nil)
		key = client.ObjectKeyFromObject(hc)
		build(owningCluster(nil))

		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		out := &infrav1.HydraCluster{}
		Expect(c.Get(ctx, key, out)).To(Succeed())
		Expect(out.Finalizers).To(BeEmpty())
		Expect(out.Annotations).NotTo(HaveKey(infrav1.NetworkOwnerAnnotation))
	})

	It("releases the network it created once nothing uses it", func() {
		ownedAndCreated()

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.DeletedNetworks).To(Equal([]string{testNetName}))
		Expect(gone()).To(BeTrue(), "the finalizer is released")
	})

	It("does not wait for machines that have no domain", func() {
		// The wedge a machine count made: deleting the HydraCluster on its own
		// while a MachineDeployment still stamps machines. Each new HydraMachine
		// is held at the create gate, never gets a domain, and never goes away.
		// One that has a domain is held by the backend's scan instead.
		machine := newMachine(func(m *infrav1.HydraMachine) {
			m.Labels = map[string]string{clusterv1.ClusterNameLabel: linkClusterName}
		})
		ownedAndCreated(owningCluster(nil), machine)

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.DeletedNetworks).To(Equal([]string{testNetName}))
		Expect(gone()).To(BeTrue())
	})

	It("waits while a machine still references the network", func() {
		ownedAndCreated()
		provider.NetworkUsers[testNetName] = true

		res, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(requeueClusterDeleting))
		_, ok := provider.HasNetwork(testNetName)
		Expect(ok).To(BeTrue())
		Expect(gone()).To(BeFalse())

		// Said on the object, not only in a log: nothing but those domains going
		// ends this wait.
		out := &infrav1.HydraCluster{}
		Expect(c.Get(ctx, key, out)).To(Succeed())
		ready := apimeta.FindStatusCondition(out.Status.Conditions, infrav1.ClusterReadyCondition)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("Deleting"))
		Expect(ready.Message).To(ContainSubstring("still in use"))
	})

	It("leaves a network it adopted, and does not hold the deletion over it", func() {
		ownedAndCreated()
		provider.AddNetwork(testNetName, "") // an operator's network: no record

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		_, ok := provider.HasNetwork(testNetName)
		Expect(ok).To(BeTrue(), "an adopted network is never removed")
		Expect(gone()).To(BeTrue())
	})

	It("leaves a network made for a previous cluster of the same name", func() {
		ownedAndCreated()
		provider.AddNetwork(testNetName, "0b9c1a64-1111-4222-8333-444455556666")

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		recorded, ok := provider.HasNetwork(testNetName)
		Expect(ok).To(BeTrue())
		Expect(recorded).NotTo(Equal(testNetworkOwnerID))
		Expect(gone()).To(BeTrue())
	})

	It("leaves a network another cluster declares, without asking the backend", func() {
		other := hydraCluster(declaring)
		other.Name, other.Namespace, other.UID = otherClusterName, "elsewhere", types.UID("uid-other")
		ownedAndCreated(other)

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.NetworkDeletes).To(BeZero())
		_, ok := provider.HasNetwork(testNetName)
		Expect(ok).To(BeTrue())
		Expect(gone()).To(BeTrue())
	})

	It("does not count another cluster that is itself being deleted", func() {
		other := hydraCluster(declaring)
		other.Name, other.UID = otherClusterName, types.UID("uid-other")
		other.Finalizers = []string{"test/hold"}
		now := metav1.Now()
		other.DeletionTimestamp = &now
		ownedAndCreated(other)

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.DeletedNetworks).To(Equal([]string{testNetName}))
	})

	It("releases a network that is already gone", func() {
		ownedAndCreated()
		Expect(provider.DeleteNetwork(ctx, testNetName, providers.NetworkOwner{ID: testNetworkOwnerID})).To(Succeed())

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		Expect(gone()).To(BeTrue())
	})

	It("keeps the finalizer when the backend fails, and tries again", func() {
		ownedAndCreated()
		provider.DeleteNetworkErr = errors.New("hypervisor unreachable")

		_, err := deleted()
		Expect(err).To(HaveOccurred())
		Expect(gone()).To(BeFalse())

		provider.DeleteNetworkErr = nil
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(gone()).To(BeTrue())
	})

	It("leaves the network when no backend can be built, rather than wedge", func() {
		ownedAndCreated()
		r.Provider = nil // nothing configured: a terminal provider error

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		_, ok := provider.HasNetwork(testNetName)
		Expect(ok).To(BeTrue())
		Expect(gone()).To(BeTrue())
	})

	It("touches nothing while paused", func() {
		ownedAndCreated()
		hc.Annotations[clusterv1.PausedAnnotation] = ""
		Expect(c.Update(ctx, hc)).To(Succeed())

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.NetworkDeletes).To(BeZero())
		Expect(gone()).To(BeFalse())
	})

	It("releases a cluster that never recorded an owner without asking the backend", func() {
		ownedAndCreated()
		delete(hc.Annotations, infrav1.NetworkOwnerAnnotation)
		Expect(c.Update(ctx, hc)).To(Succeed())

		_, err := deleted()
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.NetworkDeletes).To(BeZero())
		Expect(gone()).To(BeTrue())
	})
})

var _ = Describe("Machine creation while its HydraCluster is deleted", func() {
	It("waits rather than racing the cluster's teardown", func() {
		hc := hydraCluster(nil)
		now := metav1.Now()
		hc.DeletionTimestamp = &now
		link := &linkage{
			machine: &clusterv1.Machine{}, clusterName: linkClusterName, cluster: readyCluster(),
			hydraClusterName: linkClusterName, hydraCluster: hc,
		}
		err := clusterReadyForCreate(link)
		Expect(errors.Is(err, errWaitingForHydraCluster)).To(BeTrue(), fmt.Sprint(err))
		Expect(err.Error()).To(ContainSubstring("being deleted"))
	})
})
