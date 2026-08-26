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

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
)

// These exercise the CRD schema against a real API server, which is the only
// way to prove the CEL rules and structural validation actually behave as the
// markers claim. A unit test on the Go types would prove nothing: none of this
// validation lives in Go.

// Extracted so the linter does not flag repeated literals, and so the fixtures
// read as one shared machine shape rather than several coincidentally equal ones.
const (
	testNetwork = "br0"
	testImage   = "ubuntu-24.04"
	// A real sha256 digest (of the empty input), so the fixture exercises the
	// exact-length rule rather than a placeholder that only happens to be hex.
	testChecksum = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var uniq int

func newMachine(mutate func(*infrav1.HydraMachine)) *infrav1.HydraMachine {
	uniq++
	m := &infrav1.HydraMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("machine-%d", uniq),
			Namespace: "default",
		},
		Spec: infrav1.HydraMachineSpec{
			VCPUs:    2,
			Memory:   resource.MustParse("4Gi"),
			DiskSize: resource.MustParse("40Gi"),
			Image:    infrav1.HydraMachineImage{Name: testImage},
			Networks: []infrav1.HydraMachineNetworkAttachment{{Name: testNetwork}},
		},
	}
	if mutate != nil {
		mutate(m)
	}
	return m
}

func newTemplate(mutate func(*infrav1.HydraMachineTemplate)) *infrav1.HydraMachineTemplate {
	uniq++
	t := &infrav1.HydraMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("template-%d", uniq),
			Namespace: "default",
		},
		Spec: infrav1.HydraMachineTemplateSpec{
			Template: infrav1.HydraMachineTemplateResource{
				Spec: newMachine(nil).Spec,
			},
		},
	}
	if mutate != nil {
		mutate(t)
	}
	return t
}

var _ = Describe("HydraMachine API", func() {
	ctx := context.Background()

	Context("a well-formed machine", func() {
		It("is accepted", func() {
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		})
	})

	Context("capacity validation", func() {
		It("rejects zero vcpus", func() {
			err := k8sClient.Create(ctx, newMachine(func(m *infrav1.HydraMachine) {
				m.Spec.VCPUs = 0
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("vcpus"))
		})
	})

	Context("image validation", func() {
		It("rejects an image with neither name nor url", func() {
			err := k8sClient.Create(ctx, newMachine(func(m *infrav1.HydraMachine) {
				m.Spec.Image = infrav1.HydraMachineImage{}
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("one of name or url must be set"))
		})

		It("accepts an image given only by url", func() {
			m := newMachine(func(m *infrav1.HydraMachine) {
				m.Spec.Image = infrav1.HydraMachineImage{
					URL:      "https://example.invalid/ubuntu-24.04.img",
					Checksum: testChecksum,
				}
			})
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			Expect(k8sClient.Delete(ctx, m)).To(Succeed())
		})

		It("rejects an unsupported checksum algorithm", func() {
			err := k8sClient.Create(ctx, newMachine(func(m *infrav1.HydraMachine) {
				m.Spec.Image.URL = "https://example.invalid/img"
				m.Spec.Image.Checksum = "md5:d41d8cd98f00b204e9800998ecf8427e"
			}))
			Expect(err).To(HaveOccurred())
		})

		It("rejects a truncated sha256 digest", func() {
			// The old pattern allowed any non-empty hex run, so "sha256:a" passed
			// admission and only failed much later during provisioning.
			err := k8sClient.Create(ctx, newMachine(func(m *infrav1.HydraMachine) {
				m.Spec.Image.URL = "https://example.invalid/img"
				m.Spec.Image.Checksum = "sha256:abc123"
			}))
			Expect(err).To(HaveOccurred())
		})

		It("rejects a checksum with no url to verify", func() {
			err := k8sClient.Create(ctx, newMachine(func(m *infrav1.HydraMachine) {
				m.Spec.Image = infrav1.HydraMachineImage{Name: testImage, Checksum: testChecksum}
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("checksum is only meaningful alongside url"))
		})
	})

	Context("networks", func() {
		It("requires at least one attachment", func() {
			err := k8sClient.Create(ctx, newMachine(func(m *infrav1.HydraMachine) {
				m.Spec.Networks = nil
			}))
			Expect(err).To(HaveOccurred())
		})

		It("rejects duplicate attachments by name", func() {
			err := k8sClient.Create(ctx, newMachine(func(m *infrav1.HydraMachine) {
				m.Spec.Networks = []infrav1.HydraMachineNetworkAttachment{{Name: testNetwork}, {Name: testNetwork}}
			}))
			Expect(err).To(HaveOccurred())
		})
	})

	Context("immutability", func() {
		It("rejects a change to vcpus", func() {
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			m.Spec.VCPUs = 4
			err := k8sClient.Update(ctx, m)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})

		It("rejects a change to memory", func() {
			// Quantity compiles to x-kubernetes-int-or-string, a different schema
			// shape from a scalar, so its transition rule needs proving separately.
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			m.Spec.Memory = resource.MustParse("8Gi")
			err := k8sClient.Update(ctx, m)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})

		It("rejects a change to diskSize", func() {
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			m.Spec.DiskSize = resource.MustParse("80Gi")
			err := k8sClient.Update(ctx, m)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})

		It("rejects a change to networks", func() {
			// networks is an associative list (listType=map), which again compiles
			// differently from both scalars and quantities.
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			m.Spec.Networks = []infrav1.HydraMachineNetworkAttachment{{Name: "br1"}}
			err := k8sClient.Update(ctx, m)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})

		It("rejects appending a network", func() {
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			m.Spec.Networks = append(m.Spec.Networks, infrav1.HydraMachineNetworkAttachment{Name: "br1"})
			Expect(k8sClient.Update(ctx, m)).ToNot(Succeed())
		})

		It("rejects a change to the image", func() {
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			m.Spec.Image.Name = "ubuntu-22.04"
			Expect(k8sClient.Update(ctx, m)).ToNot(Succeed())
		})

		It("allows providerID to be set once, then freezes it", func() {
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			// The controller sets it after the backing machine exists.
			first := "hydra://libvirt/abc123"
			m.Spec.ProviderID = &first
			Expect(k8sClient.Update(ctx, m)).To(Succeed())

			// A second, different value must not be accepted.
			second := "hydra://libvirt/def456"
			m.Spec.ProviderID = &second
			err := k8sClient.Update(ctx, m)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("providerID is immutable"))
		})
	})

	Context("status", func() {
		It("records provisioning, addresses and failure domain", func() {
			m := newMachine(nil)
			Expect(k8sClient.Create(ctx, m)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, m) }()

			provisioned := true
			m.Status.Initialization.Provisioned = &provisioned
			m.Status.FailureDomain = "workstation"
			m.Status.Conditions = []metav1.Condition{{
				Type:               infrav1.MachineReadyCondition,
				Status:             metav1.ConditionTrue,
				Reason:             "Provisioned",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, m)).To(Succeed())

			fetched := &infrav1.HydraMachine{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), fetched)).To(Succeed())
			Expect(fetched.Status.Initialization.Provisioned).ToNot(BeNil())
			Expect(*fetched.Status.Initialization.Provisioned).To(BeTrue())
			Expect(fetched.Status.FailureDomain).To(Equal("workstation"))
		})
	})
})

var _ = Describe("HydraMachineTemplate API", func() {
	ctx := context.Background()

	It("accepts a well-formed template", func() {
		t := newTemplate(nil)
		Expect(k8sClient.Create(ctx, t)).To(Succeed())
		Expect(k8sClient.Delete(ctx, t)).To(Succeed())
	})

	It("rejects providerID on a template", func() {
		pid := "hydra://libvirt/should-not-be-here"
		err := k8sClient.Create(ctx, newTemplate(func(t *infrav1.HydraMachineTemplate) {
			t.Spec.Template.Spec.ProviderID = &pid
		}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("providerID must not be set on a template"))
	})

	It("rejects any change to the template spec", func() {
		t := newTemplate(nil)
		Expect(k8sClient.Create(ctx, t)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, t) }()

		t.Spec.Template.Spec.VCPUs = 8
		err := k8sClient.Update(ctx, t)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("immutable"))
	})

	It("carries the machine payload at spec.template.spec so it can be cloned", func() {
		t := newTemplate(nil)
		Expect(k8sClient.Create(ctx, t)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, t) }()

		fetched := &infrav1.HydraMachineTemplate{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(t), fetched)).To(Succeed())

		// This is the subtree Cluster API clones verbatim into a HydraMachine.
		cloned := fetched.Spec.Template.Spec
		Expect(cloned.VCPUs).To(Equal(int32(2)))
		Expect(cloned.Image.Name).To(Equal(testImage))
		Expect(cloned.Networks).To(HaveLen(1))
		Expect(cloned.ProviderID).To(BeNil())
	})
})
