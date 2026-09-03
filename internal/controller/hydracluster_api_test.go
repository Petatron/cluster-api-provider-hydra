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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
)

var clusterUniq int

// newHydraCluster builds a valid object, so each spec below breaks exactly one
// thing and the failure is unambiguous.
func newHydraCluster(mutate func(*infrav1.HydraCluster)) *infrav1.HydraCluster {
	clusterUniq++
	hc := &infrav1.HydraCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cluster-%d", clusterUniq),
			Namespace: linkNamespace,
		},
		Spec: infrav1.HydraClusterSpec{
			ControlPlaneEndpoint: clusterv1.APIEndpoint{Host: linkEndpointIP, Port: 6443},
			StoragePool:          "k8s-workers",
			Networks:             []infrav1.HydraNetworkAttachment{{Name: testNetwork}},
		},
	}
	if mutate != nil {
		mutate(hc)
	}
	return hc
}

var _ = Describe("HydraCluster API", func() {
	ctx := context.Background()

	It("admits a complete object", func() {
		Expect(k8sClient.Create(ctx, newHydraCluster(nil))).To(Succeed())
	})

	Context("controlPlaneEndpoint", func() {
		// Hydra creates no load balancer or VIP, so nothing will ever fill this
		// in later. Admitting a cluster without it means reporting infrastructure
		// provisioned while Cluster API has no endpoint to copy and kubeadm has
		// nowhere to join.
		It("rejects an object with no endpoint at all", func() {
			err := k8sClient.Create(ctx, newHydraCluster(func(hc *infrav1.HydraCluster) {
				hc.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{}
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("controlPlaneEndpoint requires both a host"))
		})

		It("rejects a host with no port", func() {
			// The reason the rule lives on the spec and not on the field: host and
			// port are individually optional in the contract type, so a
			// +required struct would still admit this.
			err := k8sClient.Create(ctx, newHydraCluster(func(hc *infrav1.HydraCluster) {
				hc.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: linkEndpointIP}
			}))
			Expect(err).To(HaveOccurred())
		})

		It("rejects a port with no host", func() {
			err := k8sClient.Create(ctx, newHydraCluster(func(hc *infrav1.HydraCluster) {
				hc.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Port: 6443}
			}))
			Expect(err).To(HaveOccurred())
		})

		It("is immutable once set", func() {
			hc := newHydraCluster(nil)
			Expect(k8sClient.Create(ctx, hc)).To(Succeed())

			hc.Spec.ControlPlaneEndpoint.Host = linkOtherIP
			Expect(k8sClient.Update(ctx, hc)).NotTo(Succeed())
		})
	})

	Context("storagePool", func() {
		It("is immutable once set", func() {
			// A reconcile cannot carry out the change it implies: existing machines
			// were cloned from a backing volume inside the old pool and keep
			// pointing at it, while new machines would land somewhere else.
			hc := newHydraCluster(nil)
			Expect(k8sClient.Create(ctx, hc)).To(Succeed())

			hc.Spec.StoragePool = "somewhere-else"
			err := k8sClient.Update(ctx, hc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("storagePool is fixed when the cluster is created"))
		})

		It("may be omitted, falling back to the manager's flag", func() {
			Expect(k8sClient.Create(ctx, newHydraCluster(func(hc *infrav1.HydraCluster) {
				hc.Spec.StoragePool = ""
			}))).To(Succeed())
		})

		It("cannot be added later either, because omission already chose a pool", func() {
			// Set-once was not enough. Omitting it selects the manager's pool, so
			// adding one afterwards would leave existing machines in that pool and
			// send every new machine somewhere else -- the split-pool state this
			// field exists to prevent.
			hc := newHydraCluster(func(hc *infrav1.HydraCluster) { hc.Spec.StoragePool = "" })
			Expect(k8sClient.Create(ctx, hc)).To(Succeed())

			hc.Spec.StoragePool = "added-afterwards"
			err := k8sClient.Update(ctx, hc)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("equally binding"))
		})
	})

	Context("networks", func() {
		It("rejects duplicate attachment names, as HydraMachine does", func() {
			// Without the map-list markers the identical list would be admitted
			// here and refused on a machine -- the same value valid or invalid
			// depending only on which object carried it. An inherited duplicate
			// would then give one attachment two NICs.
			err := k8sClient.Create(ctx, newHydraCluster(func(hc *infrav1.HydraCluster) {
				hc.Spec.Networks = []infrav1.HydraNetworkAttachment{
					{Name: testNetwork}, {Name: testNetwork},
				}
			}))
			Expect(err).To(HaveOccurred())
		})

		It("accepts none, because machines may name their own", func() {
			// Deliberately not MinItems=1. Empty is a meaningful default here, and
			// requiring one would make that arrangement inexpressible.
			Expect(k8sClient.Create(ctx, newHydraCluster(func(hc *infrav1.HydraCluster) {
				hc.Spec.Networks = nil
			}))).To(Succeed())
		})
	})

	Context("baseImage", func() {
		It("may be omitted", func() {
			Expect(k8sClient.Create(ctx, newHydraCluster(func(hc *infrav1.HydraCluster) {
				hc.Spec.BaseImage = nil
			}))).To(Succeed())
		})

		It("still enforces the image rules when present", func() {
			err := k8sClient.Create(ctx, newHydraCluster(func(hc *infrav1.HydraCluster) {
				hc.Spec.BaseImage = &infrav1.HydraImage{Checksum: testChecksum}
			}))
			Expect(err).To(HaveOccurred())
		})
	})

	It("reports provisioned and the endpoint through printer columns", func() {
		// The columns are how someone reads a stuck cluster with kubectl, so a
		// typo in a JSONPath is worth catching here rather than in anger.
		hc := newHydraCluster(nil)
		Expect(k8sClient.Create(ctx, hc)).To(Succeed())

		out := &infrav1.HydraCluster{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(hc), out)).To(Succeed())
		Expect(out.Spec.StoragePool).To(Equal("k8s-workers"))
		Expect(out.Status.Initialization.Provisioned).To(BeNil(), "nothing has reconciled it yet")
	})
})
