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

		It("hands the declared managed network to the backend", func() {
			hc = hydraCluster(func(c *infrav1.HydraCluster) {
				c.Spec.ControlPlaneEndpoint.Host = testStatedHost
				c.Spec.ManagedNetwork = &infrav1.HydraManagedNetwork{
					Name: testNetName, Subnet: testSubnet, DHCPStart: testDHCPStart, DHCPEnd: testDHCPEnd,
				}
			})
			key = client.ObjectKeyFromObject(hc)
			r := build(owningCluster(nil))

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.LastInfrastructure.ManagedNetwork).To(Equal(&providers.ManagedNetwork{
				Name: testNetName, Subnet: testSubnet, DHCPStart: testDHCPStart, DHCPEnd: testDHCPEnd,
			}))
			Expect(reload(r).Spec.ControlPlaneEndpoint).To(Equal(clusterv1.APIEndpoint{Host: testStatedHost, Port: 6443}),
				"a stated endpoint is never replaced by a derived one")
		})

		// PET-42: on a managed network the endpoint may be left for Hydra to choose.
		Context("choosing the endpoint on a managed network", func() {
			onManagedNetwork := func(endpoint clusterv1.APIEndpoint, subnet, start, end string) {
				hc = hydraCluster(func(c *infrav1.HydraCluster) {
					c.Spec.ControlPlaneEndpoint = endpoint
					c.Spec.ManagedNetwork = &infrav1.HydraManagedNetwork{
						Name: testNetName, Subnet: subnet, DHCPStart: start, DHCPEnd: end,
					}
				})
				key = client.ObjectKeyFromObject(hc)
			}

			It("records the first usable address outside the range, with port 6443, before reporting provisioned", func() {
				onManagedNetwork(clusterv1.APIEndpoint{}, testSubnet, testDHCPStart, testDHCPEnd)
				r := build(owningCluster(nil))

				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())

				out := reload(r)
				Expect(out.Spec.ControlPlaneEndpoint).To(Equal(clusterv1.APIEndpoint{Host: testDerivedHost, Port: 6443}))
				// Cluster API copies the endpoint only once provisioned is true, so
				// both must be there after the same pass.
				Expect(*out.Status.Initialization.Provisioned).To(BeTrue())
				Expect(provider.InfrastructureChecks).To(Equal(1))
			})

			It("keeps a port that was stated", func() {
				onManagedNetwork(clusterv1.APIEndpoint{Port: 7443}, testSubnet, testDHCPStart, testDHCPEnd)
				r := build(owningCluster(nil))

				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(reload(r).Spec.ControlPlaneEndpoint).To(Equal(clusterv1.APIEndpoint{Host: testDerivedHost, Port: 7443}))
			})

			It("keeps the recorded address on later passes", func() {
				onManagedNetwork(clusterv1.APIEndpoint{}, testSubnet, testDHCPStart, testDHCPEnd)
				r := build(owningCluster(nil))

				for range 3 {
					_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
					Expect(err).NotTo(HaveOccurred())
				}
				out := reload(r)
				Expect(out.Spec.ControlPlaneEndpoint.Host).To(Equal(testDerivedHost))
				Expect(out.Generation).To(BeNumerically("<=", 1), "the spec is written once, not on every pass")
			})

			It("refuses a network with no address left, without recording anything or asking the backend", func() {
				// A /29: .0 network, .1 gateway, .7 broadcast, and .2-.6 all handed out.
				onManagedNetwork(clusterv1.APIEndpoint{}, "192.168.60.0/29", testDerivedHost, "192.168.60.6")
				r := build(owningCluster(nil))

				res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(requeueClusterUnverified))
				Expect(provider.InfrastructureChecks).To(BeZero())

				out := reload(r)
				Expect(out.Spec.ControlPlaneEndpoint).To(Equal(clusterv1.APIEndpoint{}))
				Expect(*out.Status.Initialization.Provisioned).To(BeFalse())
				failed := condition(r, infrav1.ClusterInfrastructureFailedCondition)
				Expect(failed).NotTo(BeNil())
				Expect(failed.Message).To(ContainSubstring("no address left for the control-plane endpoint"))
			})

			It("chooses nothing while the cluster is paused", func() {
				onManagedNetwork(clusterv1.APIEndpoint{}, testSubnet, testDHCPStart, testDHCPEnd)
				paused := true
				r := build(owningCluster(func(c *clusterv1.Cluster) { c.Spec.Paused = &paused }))

				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(reload(r).Spec.ControlPlaneEndpoint).To(Equal(clusterv1.APIEndpoint{}))
			})

			// Cluster API uses an endpoint stated on the Cluster instead of copying
			// this one. Choosing a different address here would record, for good,
			// an endpoint the cluster does not have.
			It("records the endpoint the Cluster states instead of choosing one", func() {
				onManagedNetwork(clusterv1.APIEndpoint{}, testSubnet, testDHCPStart, testDHCPEnd)
				stated := clusterv1.APIEndpoint{Host: "192.168.60.20", Port: 6443}
				r := build(owningCluster(func(c *clusterv1.Cluster) { c.Spec.ControlPlaneEndpoint = stated }))

				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(reload(r).Spec.ControlPlaneEndpoint).To(Equal(stated))
			})

			It("refuses an endpoint the Cluster states inside the range, recording nothing", func() {
				onManagedNetwork(clusterv1.APIEndpoint{}, testSubnet, testDHCPStart, testDHCPEnd)
				r := build(owningCluster(func(c *clusterv1.Cluster) {
					c.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "192.168.60.150", Port: 6443}
				}))

				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(reload(r).Spec.ControlPlaneEndpoint).To(Equal(clusterv1.APIEndpoint{}))
				failed := condition(r, infrav1.ClusterInfrastructureFailedCondition)
				Expect(failed).NotTo(BeNil())
				Expect(failed.Message).To(ContainSubstring("DHCP range"))
			})

			It("refuses a port that disagrees with the Cluster's endpoint", func() {
				onManagedNetwork(clusterv1.APIEndpoint{Port: 7443}, testSubnet, testDHCPStart, testDHCPEnd)
				r := build(owningCluster(func(c *clusterv1.Cluster) {
					c.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: "192.168.60.20", Port: 6443}
				}))

				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(reload(r).Spec.ControlPlaneEndpoint).To(Equal(clusterv1.APIEndpoint{Port: 7443}))
				Expect(condition(r, infrav1.ClusterInfrastructureFailedCondition).Message).To(ContainSubstring("states port 7443"))
			})

			It("records nothing until it knows its owning Cluster", func() {
				onManagedNetwork(clusterv1.APIEndpoint{}, testSubnet, testDHCPStart, testDHCPEnd)
				hc.OwnerReferences = nil
				r := build()

				res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(Equal(requeueClusterUnverified))
				Expect(provider.InfrastructureChecks).To(BeZero())
				Expect(reload(r).Spec.ControlPlaneEndpoint).To(Equal(clusterv1.APIEndpoint{}))
				ready := condition(r, infrav1.ClusterReadyCondition)
				Expect(ready.Status).To(Equal(metav1.ConditionFalse))
				Expect(ready.Message).To(ContainSubstring("waiting for the owning Cluster"))
				Expect(condition(r, infrav1.ClusterInfrastructureFailedCondition)).To(BeNil(), "waiting is not a failure")
			})
		})

		It("refuses an endpoint the managed network would hand out, before asking the backend", func() {
			// The backend would otherwise create the network for a cluster that
			// can never work on it.
			hc = hydraCluster(func(c *infrav1.HydraCluster) {
				c.Spec.ControlPlaneEndpoint.Host = "192.168.60.150"
				c.Spec.ManagedNetwork = &infrav1.HydraManagedNetwork{
					Name: testNetName, Subnet: testSubnet, DHCPStart: testDHCPStart, DHCPEnd: testDHCPEnd,
				}
			})
			key = client.ObjectKeyFromObject(hc)
			r := build(owningCluster(nil))

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueClusterUnverified))
			Expect(provider.InfrastructureChecks).To(BeZero())
			failed := condition(r, infrav1.ClusterInfrastructureFailedCondition)
			Expect(failed).NotTo(BeNil())
			Expect(failed.Message).To(ContainSubstring("DHCP range"))
		})
	})

	Context("backend availability", func() {
		It("raises InfrastructureFailed when no backend is configured", func() {
			r := build(owningCluster(nil))
			r.Provider = nil

			// Terminal, so handled like a terminal infrastructure failure: the
			// condition says an operator must act, and the reconcile requeues
			// slowly instead of retrying with backoff forever.
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueClusterHealthy))
			failed := condition(r, infrav1.ClusterInfrastructureFailedCondition)
			Expect(failed).NotTo(BeNil())
			Expect(failed.Status).To(Equal(metav1.ConditionTrue))
		})

		It("reports a backend that cannot be built, retries, and asks for its own cluster", func() {
			attempts := 0
			r := build(owningCluster(nil))
			r.Provider = nil
			var asked []string
			r.ProviderFor = func(_ context.Context, owner providers.Owner, hypervisor *providers.Hypervisor) (providers.MachineProvider, error) {
				attempts++
				Expect(hypervisor).To(BeNil(), "a cluster naming no hypervisor uses the manager's flag connection")
				asked = append(asked, owner.Name)
				if attempts == 1 {
					return nil, errors.New("hypervisor unreachable")
				}
				return provider, nil
			}

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())
			ready := condition(r, infrav1.ClusterReadyCondition)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Message).To(ContainSubstring("hypervisor unreachable"))
			Expect(condition(r, infrav1.ClusterInfrastructureFailedCondition)).To(BeNil(),
				"an unreachable hypervisor is not a configuration error")

			for range 2 {
				_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
			// Connections are held by whatever implements ProviderFor (a pool, in
			// the manager), so the reconciler asks on every reconcile -- for the
			// cluster it is reconciling, since clusters may name different
			// hypervisors (PET-38).
			Expect(attempts).To(Equal(3))
			Expect(asked).To(Equal([]string{key.Name, key.Name, key.Name}))
			Expect(provider.InfrastructureChecks).To(Equal(2))
		})

		It("releases a deleted cluster's connection", func() {
			r := build(owningCluster(nil))
			var released []providers.Owner
			r.ReleaseProvider = func(_ context.Context, owner providers.Owner) {
				released = append(released, owner)
			}

			// Gone entirely: there is no finalizer, so this is the usual path.
			Expect(r.Delete(ctx, hc)).To(Succeed())
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(released).To(Equal([]providers.Owner{{Kind: hydraClusterKind, NamespacedName: key}}))
		})

		It("releases a cluster's connection once it is being deleted", func() {
			hc.Finalizers = []string{"test.hydra/hold"}
			r := build(owningCluster(nil))
			var released []providers.Owner
			r.ReleaseProvider = func(_ context.Context, owner providers.Owner) {
				released = append(released, owner)
			}
			Expect(r.Delete(ctx, hc)).To(Succeed())
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(released).To(Equal([]providers.Owner{{Kind: hydraClusterKind, NamespacedName: key}}))
			Expect(provider.InfrastructureChecks).To(Equal(0), "a cluster being deleted is not verified")
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
		Expect(validateEndpointAgainstManagedNetwork(with(testStatedHost, net(nil)))).To(Succeed())
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

var _ = Describe("choosing a managed network's endpoint", func() {
	// The inverse of the invariant above: the first address it would accept.
	network := func(subnet, start, end string) *infrav1.HydraManagedNetwork {
		return &infrav1.HydraManagedNetwork{Name: testNetName, Subnet: subnet, DHCPStart: start, DHCPEnd: end}
	}

	DescribeTable("picks the first usable address outside the DHCP range",
		func(n *infrav1.HydraManagedNetwork, want string) {
			got, err := deriveEndpointHost(n)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
			// Whatever it picks, the check every stated endpoint gets must pass.
			hc := &infrav1.HydraCluster{}
			hc.Spec.ManagedNetwork = n
			hc.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: got, Port: defaultAPIServerPort}
			Expect(validateEndpointAgainstManagedNetwork(hc)).To(Succeed())
		},
		Entry("the ticket's example: .0 network, .1 gateway, so .2",
			network(testSubnet, testDHCPStart, testDHCPEnd), testDerivedHost),
		Entry("a range starting at .2 pushes it past the range's end",
			network(testSubnet, testDerivedHost, "192.168.60.200"), "192.168.60.201"),
		Entry("a range covering the gateway too",
			network(testSubnet, "192.168.60.1", "192.168.60.50"), "192.168.60.51"),
		Entry("a range that ends one short of the broadcast address",
			network(testSubnet, testDerivedHost, "192.168.60.253"), "192.168.60.254"),
		Entry("a /30 whose only host is not in the range",
			network(testSmallNet, "192.168.60.0", "192.168.60.0"), testSmallHost),
		Entry("a subnet that does not start at .0 of a /24",
			network("10.20.30.64/26", "10.20.30.100", "10.20.30.120"), "10.20.30.66"),
	)

	DescribeTable("refuses a network with no address left",
		func(n *infrav1.HydraManagedNetwork) {
			_, err := deriveEndpointHost(n)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue(), "the network is immutable, so waiting cannot help")
			Expect(err.Error()).To(ContainSubstring("no address left"))
		},
		Entry("a range covering every host after the gateway",
			network(testSubnet, testDerivedHost, "192.168.60.254")),
		Entry("a /30 whose only host is in the range",
			network(testSmallNet, testSmallHost, testSmallHost)),
	)

	It("refuses DHCP bounds that are not addresses", func() {
		_, err := deriveEndpointHost(network(testSubnet, "not-an-address", testDHCPEnd))
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue())
	})
})
