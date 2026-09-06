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
	linkPool      = "k8s-workers"
	linkBaseImage = "ubuntu-24.04-server-cloudimg-amd64.img"
)

// hydraCluster is the infrastructure object a Cluster points at.
func hydraCluster(mutate func(*infrav1.HydraCluster)) *infrav1.HydraCluster {
	hc := &infrav1.HydraCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      linkClusterName,
			Namespace: linkNamespace,
			UID:       types.UID("uid-hydracluster"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       clusterKind,
				Name:       linkClusterName,
				UID:        types.UID("uid-cluster"),
			}},
		},
		Spec: infrav1.HydraClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: linkEndpointIP, Port: 6443},
			StoragePool:          linkPool,
			BaseImage:            &infrav1.HydraImage{Name: linkBaseImage},
			Networks:             []infrav1.HydraNetworkAttachment{{Name: testNetwork}},
		},
	}
	if mutate != nil {
		mutate(hc)
	}
	return hc
}

// verifiedHydraCluster is a HydraCluster that has already been reconciled and
// found good.
//
// Kept separate from hydraCluster() rather than folded into it: the cluster
// reconciler specs need an unverified starting point to prove they do the
// verifying, while the machine specs need a verified one because creation now
// gates on current readiness rather than on the latched milestone.
func verifiedHydraCluster(mutate func(*infrav1.HydraCluster)) *infrav1.HydraCluster {
	hc := hydraCluster(mutate)
	hc.Generation = 1
	provisioned := true
	hc.Status.Initialization.Provisioned = &provisioned
	hc.Status.Conditions = []metav1.Condition{{
		Type:               infrav1.ClusterReadyCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "InfrastructureVerified",
		Message:            "storage pool is running and the base image is present in it",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: 1,
	}}
	return hc
}

// owningCluster is the Cluster that points back at the HydraCluster above.
func owningCluster(mutate func(*clusterv1.Cluster)) *clusterv1.Cluster {
	c := readyCluster()
	c.UID = types.UID("uid-cluster")
	c.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: infrav1.GroupVersion.Group,
		Kind:     hydraClusterKind,
		Name:     linkClusterName,
	}
	if mutate != nil {
		mutate(c)
	}
	return c
}

var _ = Describe("HydraCluster Reconciler", func() {
	ctx := context.Background()

	var (
		provider *fake.Provider
		hc       *infrav1.HydraCluster
		key      types.NamespacedName
	)

	build := func(objs ...client.Object) *HydraClusterReconciler {
		s := linkScheme()
		all := append([]client.Object{hc}, objs...)
		c := fakeclient.NewClientBuilder().
			WithScheme(s).
			WithObjects(all...).
			WithStatusSubresource(&infrav1.HydraCluster{}).
			Build()
		return &HydraClusterReconciler{Client: c, Scheme: s, Provider: provider}
	}

	reload := func(r *HydraClusterReconciler) *infrav1.HydraCluster {
		out := &infrav1.HydraCluster{}
		Expect(r.Get(ctx, key, out)).To(Succeed())
		return out
	}

	condition := func(r *HydraClusterReconciler, t string) *metav1.Condition {
		return apimeta.FindStatusCondition(reload(r).Status.Conditions, t)
	}

	BeforeEach(func() {
		provider = fake.New()
		hc = hydraCluster(nil)
		key = client.ObjectKeyFromObject(hc)
	})

	Context("verification", func() {
		It("checks the pool and image the cluster actually names", func() {
			// The whole point of this object: these used to be manager flags, so
			// every cluster got the same two values.
			r := build(owningCluster(nil))

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.InfrastructureChecks).To(Equal(1))
			Expect(provider.LastInfrastructure.StoragePool).To(Equal(linkPool))
			Expect(provider.LastInfrastructure.Image.Name).To(Equal(linkBaseImage))
		})

		It("reports provisioned and Ready when the infrastructure is there", func() {
			r := build(owningCluster(nil))

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueClusterHealthy))

			out := reload(r)
			Expect(out.Status.Initialization.Provisioned).NotTo(BeNil())
			Expect(*out.Status.Initialization.Provisioned).To(BeTrue())
			Expect(condition(r, infrav1.ClusterReadyCondition).Status).To(Equal(metav1.ConditionTrue))
		})

		It("does not demand an image when the cluster named no default", func() {
			// The cluster-level half of a blocking review finding. A zero Image used
			// to fall through to the manager's --libvirt-base-image and then require
			// that volume inside *this* cluster's pool -- gating a perfectly valid
			// cluster, whose machines all name their own image, on a volume nobody
			// asked it to use.
			hc = hydraCluster(func(c *infrav1.HydraCluster) { c.Spec.BaseImage = nil })
			key = client.ObjectKeyFromObject(hc)
			r := build(owningCluster(nil))

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.LastInfrastructure.Image).To(Equal(providers.Image{}),
				"a zero image tells the backend to skip the image prerequisite")

			out := reload(r)
			Expect(*out.Status.Initialization.Provisioned).To(BeTrue())

			// And the message must not claim something that was never checked.
			ready := apimeta.FindStatusCondition(out.Status.Conditions, infrav1.ClusterReadyCondition)
			Expect(ready.Message).To(Equal("storage pool is running"))
			Expect(ready.Message).NotTo(ContainSubstring("base image"))
		})

		It("says the base image was checked only when it was", func() {
			r := build(owningCluster(nil))
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			ready := apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.ClusterReadyCondition)
			Expect(ready.Message).To(ContainSubstring("base image"))
		})

		It("re-checks periodically, because a pool can be stopped later", func() {
			// Verification is a point-in-time statement. Reporting it once and never
			// looking again would leave Ready=True on a cluster whose pool has since
			// been stopped.
			r := build(owningCluster(nil))

			for i := range 3 {
				res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(requeueClusterHealthy), "pass %d", i)
			}
			Expect(provider.InfrastructureChecks).To(Equal(3))
		})
	})

	Context("failures", func() {
		It("raises InfrastructureFailed only for what will not fix itself", func() {
			provider.InfrastructureErr = fmt.Errorf("%w: storage pool %q does not exist",
				providers.ErrTerminal, linkPool)
			r := build(owningCluster(nil))

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			// Deliberately not an error return: retrying with backoff forever would
			// achieve nothing, and the condition already says a human must act.
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueClusterHealthy))

			Expect(condition(r, infrav1.ClusterReadyCondition).Status).To(Equal(metav1.ConditionFalse))
			failed := condition(r, infrav1.ClusterInfrastructureFailedCondition)
			Expect(failed).NotTo(BeNil())
			Expect(failed.Status).To(Equal(metav1.ConditionTrue))
			Expect(failed.Message).To(ContainSubstring(linkPool))
		})

		It("does not raise it for a hypervisor that is merely unreachable", func() {
			// Same misclassification the PET-8 review corrected for machines: an
			// unreachable backend is not a configuration error, and flagging it as
			// one invites intervention on something about to be fine.
			provider.InfrastructureErr = errors.New("dial tcp: connection refused")
			r := build(owningCluster(nil))

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueClusterUnverified))

			Expect(condition(r, infrav1.ClusterReadyCondition).Status).To(Equal(metav1.ConditionFalse))
			Expect(condition(r, infrav1.ClusterInfrastructureFailedCondition)).To(BeNil())
		})

		It("never regresses provisioned once it has been true", func() {
			// It is an initialization milestone Cluster API orchestrates on, not a
			// liveness signal. Flipping it back would tell CAPI the cluster is being
			// provisioned again. Ongoing health is the Ready condition's job.
			r := build(owningCluster(nil))
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(*reload(r).Status.Initialization.Provisioned).To(BeTrue())

			provider.InfrastructureErr = fmt.Errorf("%w: pool vanished", providers.ErrTerminal)
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			out := reload(r)
			Expect(*out.Status.Initialization.Provisioned).To(BeTrue(), "milestone must latch forward")
			Expect(apimeta.FindStatusCondition(out.Status.Conditions,
				infrav1.ClusterReadyCondition).Status).To(Equal(metav1.ConditionFalse))
		})
	})

	Context("pausing and deletion", func() {
		It("honours spec.paused on the owning Cluster", func() {
			paused := true
			r := build(owningCluster(func(c *clusterv1.Cluster) { c.Spec.Paused = &paused }))

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.InfrastructureChecks).To(BeZero(), "a paused cluster must not be probed")
			cond := condition(r, infrav1.ClusterPausedCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Message).To(ContainSubstring("spec.paused"))
		})

		It("proceeds when the owning Cluster cannot be read", func() {
			// Verification does not depend on the Cluster, so a missing owner costs
			// only the paused check. Blocking on it would stall a cluster whose
			// owner reference simply has not been set yet.
			r := build()

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.InfrastructureChecks).To(Equal(1))
		})

		It("deletes without a finalizer, since it creates nothing to release", func() {
			// The pool and the image were there before this object and outlive it.
			// A finalizer would guard nothing and add a way for deletion to wedge.
			now := metav1.Now()
			hc.DeletionTimestamp = &now
			hc.Finalizers = []string{"test.example.com/keep-the-fake-client-happy"}
			r := build(owningCluster(nil))

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.InfrastructureChecks).To(BeZero())
			Expect(reload(r).Finalizers).NotTo(ContainElement(ContainSubstring("hydracluster.infrastructure")))
		})
	})

	Context("watch mapping", func() {
		It("maps a Cluster to the HydraCluster it references", func() {
			reqs := clusterToHydraCluster(ctx, owningCluster(nil))
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName.Name).To(Equal(linkClusterName))
		})

		It("ignores a Cluster pointing at another provider", func() {
			other := owningCluster(func(c *clusterv1.Cluster) {
				c.Spec.InfrastructureRef.Kind = "DockerCluster"
			})
			Expect(clusterToHydraCluster(ctx, other)).To(BeEmpty())
		})
	})
})

var _ = Describe("managed network endpoint invariant", func() {
	// The point of declaring a DHCP range is that everything outside it is the
	// cluster's to allocate. Nothing else enforces that the endpoint actually
	// honours it: subnet, range and endpoint are three independently valid
	// fields, and CEL cannot order IP addresses, so a cluster asking for an
	// endpoint inside its own DHCP range admits cleanly and then hands that
	// address to the first machine that asks.
	net := func(mutate func(*infrav1.HydraManagedNetwork)) *infrav1.HydraManagedNetwork {
		n := &infrav1.HydraManagedNetwork{
			Name:      testNetName,
			Subnet:    testSubnet,
			DHCPStart: testDHCPStart,
			DHCPEnd:   testDHCPEnd,
		}
		if mutate != nil {
			mutate(n)
		}
		return n
	}
	with := func(host string, n *infrav1.HydraManagedNetwork) *infrav1.HydraCluster {
		hc := &infrav1.HydraCluster{}
		hc.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: host, Port: 6443}
		hc.Spec.ManagedNetwork = n
		return hc
	}

	It("accepts an endpoint below the range", func() {
		Expect(validateEndpointAgainstManagedNetwork(with("192.168.60.10", net(nil)))).To(Succeed())
	})

	It("accepts an endpoint above the range", func() {
		Expect(validateEndpointAgainstManagedNetwork(with("192.168.60.250", net(nil)))).To(Succeed())
	})

	It("refuses an endpoint inside the DHCP range", func() {
		err := validateEndpointAgainstManagedNetwork(with("192.168.60.150", net(nil)))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("inside managedNetwork's DHCP range"))
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue(), "immutable field, so waiting cannot help")
	})

	It("refuses the range boundaries themselves", func() {
		for _, host := range []string{testDHCPStart, testDHCPEnd} {
			Expect(validateEndpointAgainstManagedNetwork(with(host, net(nil)))).NotTo(Succeed(), host)
		}
	})

	// Outside the DHCP range is not the same as usable. These three were never
	// available, and an endpoint on any of them fails in a way that looks like
	// the VIP simply not working.
	It("refuses the gateway, network and broadcast addresses", func() {
		for host, why := range map[string]string{
			"192.168.60.0":   "network address",
			"192.168.60.1":   "gateway",
			"192.168.60.255": "broadcast address",
		} {
			err := validateEndpointAgainstManagedNetwork(with(host, net(nil)))
			Expect(err).To(HaveOccurred(), "%s is the %s", host, why)
			Expect(err.Error()).To(ContainSubstring(why))
		}
	})

	It("refuses an endpoint outside the subnet entirely", func() {
		err := validateEndpointAgainstManagedNetwork(with("192.168.15.10", net(nil)))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not inside managedNetwork.subnet"))
	})

	// A /30 is accepted by the backend's subnet check, and this is where it
	// fails for the right reason: gateway plus one host, and that host is inside
	// any DHCP range, so no endpoint is left.
	It("leaves a /30 with nowhere to put an endpoint", func() {
		small := net(func(n *infrav1.HydraManagedNetwork) {
			n.Subnet = testSmallNet
			n.DHCPStart = testSmallHost
			n.DHCPEnd = testSmallHost
		})
		Expect(validateEndpointAgainstManagedNetwork(with(testSmallHost, small))).NotTo(Succeed())
	})

	It("says nothing about a DNS name, which is the operator's to keep", func() {
		Expect(validateEndpointAgainstManagedNetwork(with("api.hydra.example", net(nil)))).To(Succeed())
	})

	It("is inert when no managed network is declared", func() {
		Expect(validateEndpointAgainstManagedNetwork(with("192.168.15.10", nil))).To(Succeed())
	})
})
