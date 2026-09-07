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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	libvirtprovider "github.com/Petatron/cluster-api-provider-hydra/internal/providers/libvirt"
)

// These specs are the scale-from-zero contract, checked the way its only
// consumer checks it.
//
// Cluster Autoscaler does not use our Go types. It reads the object as
// unstructured JSON out of a dynamic client and pulls two string maps out of
// status. Asserting on infrav1.HydraMachineTemplateStatus would pass while the
// wire form was unusable, because the Go type cannot express the thing that
// actually breaks: a value that is not a JSON string.
var _ = Describe("HydraMachineTemplate scale-from-zero contract", func() {
	ctx := context.Background()

	// publish writes the status this provider publishes, through a real API
	// server, and returns the object as the autoscaler would see it.
	publish := func(t *infrav1.HydraMachineTemplate) *unstructured.Unstructured {
		Expect(k8sClient.Create(ctx, t)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t) })

		t.Status.Capacity = capacityFor(t)
		platform := libvirtprovider.NodePlatform()
		t.Status.NodeInfo = infrav1.HydraNodeInfo{
			Architecture:    infrav1.HydraNodeArchitecture(platform.Architecture),
			OperatingSystem: platform.OperatingSystem,
		}
		Expect(k8sClient.Status().Update(ctx, t)).To(Succeed())

		raw := &unstructured.Unstructured{}
		raw.SetGroupVersionKind(infrav1.GroupVersion.WithKind("HydraMachineTemplate"))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(t), raw)).To(Succeed())
		return raw
	}

	// The autoscaler's resourceCapacityFromInfrastructureObject, verbatim in
	// shape: NestedStringMap over status.capacity. It fails whole rather than
	// per-key, so one non-string value returns no capacity at all -- and the
	// autoscaler's response to no capacity is to skip the node group silently.
	It("exposes status.capacity as a map of strings", func() {
		raw := publish(newTemplate(nil))

		capacity, found, err := unstructured.NestedStringMap(raw.Object, "status", "capacity")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "status.capacity must be readable as a string map or scale-from-zero is silently off")

		Expect(capacity).To(HaveKey(string(corev1.ResourceCPU)))
		Expect(capacity).To(HaveKey(string(corev1.ResourceMemory)))
		Expect(capacity).To(HaveKey(string(corev1.ResourceEphemeralStorage)))

		// Every value has to survive resource.ParseQuantity; the autoscaler drops
		// the ones that do not, one key at a time and without complaint.
		for name, value := range capacity {
			_, err := resource.ParseQuantity(value)
			Expect(err).NotTo(HaveOccurred(), "capacity[%s] = %q is not a parseable quantity", name, value)
		}
	})

	// CanScaleFromZero() is exactly this check. Everything else about the
	// template being right does not compensate for either of these missing.
	It("reports the cpu and memory the autoscaler requires to scale a pool at zero", func() {
		raw := publish(newTemplate(nil))

		capacity, _, err := unstructured.NestedStringMap(raw.Object, "status", "capacity")
		Expect(err).NotTo(HaveOccurred())

		cpu, err := resource.ParseQuantity(capacity[string(corev1.ResourceCPU)])
		Expect(err).NotTo(HaveOccurred())
		Expect(cpu.Value()).To(Equal(int64(2)))

		mem, err := resource.ParseQuantity(capacity[string(corev1.ResourceMemory)])
		Expect(err).NotTo(HaveOccurred())
		want := resource.MustParse("4Gi")
		Expect(mem.Value()).To(Equal(want.Value()))
	})

	// systemInfoFromInfrastructureObject, same shape, same failure mode.
	It("exposes status.nodeInfo as a map of strings", func() {
		raw := publish(newTemplate(nil))

		nodeInfo, found, err := unstructured.NestedStringMap(raw.Object, "status", "nodeInfo")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(nodeInfo["architecture"]).To(Equal("amd64"))
		Expect(nodeInfo["operatingSystem"]).To(Equal("linux"))
	})

	// The quantity schema is anyOf integer-or-string, so the API server would
	// otherwise store `cpu: 4` as a JSON number. Nothing downstream reports that:
	// NestedStringMap returns not-found, the node group is skipped, and the pool
	// simply never scales. The rule turns that into a rejected write.
	It("refuses a capacity value that is not a string", func() {
		t := newTemplate(nil)
		Expect(k8sClient.Create(ctx, t)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t) })

		raw := &unstructured.Unstructured{}
		raw.SetGroupVersionKind(infrav1.GroupVersion.WithKind("HydraMachineTemplate"))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(t), raw)).To(Succeed())

		Expect(unstructured.SetNestedField(raw.Object,
			map[string]any{"cpu": int64(4), "memory": "4Gi"}, "status", "capacity")).To(Succeed())

		err := k8sClient.Status().Update(ctx, raw)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("capacity values must be strings"))
	})

	It("rejects an architecture Kubernetes has no node label for", func() {
		t := newTemplate(nil)
		Expect(k8sClient.Create(ctx, t)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t) })

		t.Status.NodeInfo = infrav1.HydraNodeInfo{Architecture: "x86_64"}
		err := k8sClient.Status().Update(ctx, t)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("architecture"))
	})
})
