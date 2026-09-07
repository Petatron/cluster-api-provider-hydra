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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
	libvirtprovider "github.com/Petatron/cluster-api-provider-hydra/internal/providers/libvirt"
)

var _ = Describe("HydraMachineTemplate Reconciler", func() {
	ctx := context.Background()

	var (
		tmpl *infrav1.HydraMachineTemplate
		key  types.NamespacedName
	)

	build := func(platform providers.NodePlatform) *HydraMachineTemplateReconciler {
		s := linkScheme()
		c := fakeclient.NewClientBuilder().
			WithScheme(s).
			WithObjects(tmpl).
			WithStatusSubresource(&infrav1.HydraMachineTemplate{}).
			Build()
		return &HydraMachineTemplateReconciler{Client: c, Scheme: s, Platform: platform}
	}

	reload := func(r *HydraMachineTemplateReconciler) *infrav1.HydraMachineTemplate {
		out := &infrav1.HydraMachineTemplate{}
		Expect(r.Get(ctx, key, out)).To(Succeed())
		return out
	}

	BeforeEach(func() {
		tmpl = &infrav1.HydraMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: linkNamespace},
			Spec: infrav1.HydraMachineTemplateSpec{
				Template: infrav1.HydraMachineTemplateResource{
					Spec: infrav1.HydraMachineSpec{
						VCPUs:    4,
						Memory:   resource.MustParse("8Gi"),
						DiskSize: resource.MustParse("60Gi"),
						Image:    &infrav1.HydraImage{Name: testImage},
						Networks: []infrav1.HydraNetworkAttachment{{Name: testNetwork}},
					},
				},
			},
		}
		key = client.ObjectKeyFromObject(tmpl)
	})

	It("publishes the capacity a zero-replica pool is sized from", func() {
		r := build(libvirtprovider.NodePlatform())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		status := reload(r).Status
		cpu := status.Capacity[corev1.ResourceCPU]
		Expect(cpu.String()).To(Equal("4"))
		memory := status.Capacity[corev1.ResourceMemory]
		Expect(memory.String()).To(Equal("8Gi"))
		ephemeral := status.Capacity[corev1.ResourceEphemeralStorage]
		Expect(ephemeral.String()).To(Equal("60Gi"))
		Expect(status.NodeInfo.Architecture).To(Equal(infrav1.HydraNodeArchitectureAMD64))
		Expect(status.NodeInfo.OperatingSystem).To(Equal("linux"))
	})

	// Both are required for the autoscaler to consider the node group scalable
	// from zero at all -- it checks for exactly these two and skips the group
	// otherwise, so anything else being right does not compensate.
	It("always reports cpu and memory, which is what makes the pool scalable at zero", func() {
		r := build(libvirtprovider.NodePlatform())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		capacity := reload(r).Status.Capacity
		Expect(capacity).To(HaveKey(corev1.ResourceCPU))
		Expect(capacity).To(HaveKey(corev1.ResourceMemory))
	})

	// The pods entry is overwritten by the autoscaler unconditionally, from its
	// own annotation or its default of 110. Publishing one would be read and
	// discarded, which is worse than silence because it looks authoritative.
	It("does not report pods, which the autoscaler would discard", func() {
		r := build(libvirtprovider.NodePlatform())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(reload(r).Status.Capacity).NotTo(HaveKey(corev1.ResourcePods))
	})

	// This controller watches its own kind, so a status patch enqueues the object
	// it just patched. Writing unconditionally would be an unbounded write loop
	// against the API server, which is the kind of fault that only shows up as
	// someone else's rate limiting.
	It("writes nothing on a second reconcile", func() {
		r := build(libvirtprovider.NodePlatform())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		first := reload(r).ResourceVersion

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).ResourceVersion).To(Equal(first))
	})

	It("republishes capacity that was removed from status", func() {
		r := build(libvirtprovider.NodePlatform())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		wiped := reload(r)
		wiped.Status.Capacity = nil
		Expect(r.Status().Update(ctx, wiped)).To(Succeed())

		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Capacity).To(HaveKey(corev1.ResourceCPU))
	})

	// The platform is injected rather than assumed, so a backend that builds
	// arm64 machines reports arm64 without this controller changing. Getting it
	// wrong is not a cosmetic error: the autoscaler turns it into the simulated
	// node's kubernetes.io/arch label, so an amd64 default on an arm64 pool
	// schedules pods onto nodes that cannot run their images.
	It("reports the backend's architecture rather than assuming one", func() {
		r := build(providers.NodePlatform{Architecture: "arm64", OperatingSystem: "linux"})
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(reload(r).Status.NodeInfo.Architecture).To(Equal(infrav1.HydraNodeArchitectureARM64))
	})

	It("does not touch a template being deleted", func() {
		now := metav1.Now()
		tmpl.DeletionTimestamp = &now
		tmpl.Finalizers = []string{"test.hydra/keep-alive"}

		r := build(libvirtprovider.NodePlatform())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(reload(r).Status.Capacity).To(BeEmpty())
	})

	It("ignores a template that no longer exists", func() {
		r := build(libvirtprovider.NodePlatform())
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: linkNamespace, Name: "gone"},
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// resource.Quantity keeps the notation it was parsed from, so a spec applied as
// "8589934592" and one applied as "8Gi" produce structurally different but
// numerically identical quantities. Comparing with reflect.DeepEqual would find
// a difference on every reconcile and rewrite status forever.
func TestCapacityEqualComparesValuesNotNotation(t *testing.T) {
	bytes := infrav1.HydraNodeCapacity{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8589934592"),
	}
	gibibytes := infrav1.HydraNodeCapacity{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	if !capacityEqual(bytes, gibibytes) {
		t.Error("equal quantities in different notation compared unequal, which would rewrite status on every reconcile")
	}

	bigger := infrav1.HydraNodeCapacity{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("16Gi"),
	}
	if capacityEqual(bytes, bigger) {
		t.Error("different quantities compared equal, so a corrected capacity would never be republished")
	}

	if capacityEqual(bytes, infrav1.HydraNodeCapacity{corev1.ResourceCPU: resource.MustParse("4")}) {
		t.Error("maps of different length compared equal")
	}

	// A key present in one and absent in the other, with the lengths matching, is
	// the case a length check alone would miss.
	swapped := infrav1.HydraNodeCapacity{
		corev1.ResourceCPU:              resource.MustParse("4"),
		corev1.ResourceEphemeralStorage: resource.MustParse("8589934592"),
	}
	if capacityEqual(bytes, swapped) {
		t.Error("maps with the same size but different keys compared equal")
	}
}

// The libvirt backend hardcodes the domain's architecture, and the value
// reported for scale-from-zero has to be that same architecture spelled the way
// Kubernetes spells it. If the two ever disagree, the autoscaler labels a
// simulated node with an architecture the guest cannot actually run.
func TestLibvirtNodePlatformMatchesWhatTheBackendBuilds(t *testing.T) {
	p := libvirtprovider.NodePlatform()
	if p.Architecture != "amd64" {
		t.Errorf("architecture = %q, want amd64 for an x86_64 domain", p.Architecture)
	}
	if p.OperatingSystem != "linux" {
		t.Errorf("operatingSystem = %q, want linux", p.OperatingSystem)
	}
}
