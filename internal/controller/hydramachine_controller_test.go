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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers/fake"
)

// renamedProvider presents another provider under a different backend name, to
// simulate a providerID written by a different backend.
type renamedProvider struct {
	*fake.Provider
	name string
}

func (p *renamedProvider) Name() string { return p.name }

var _ = Describe("HydraMachine Reconciler", func() {
	ctx := context.Background()

	var (
		provider *fake.Provider
		r        *HydraMachineReconciler
		machine  *infrav1.HydraMachine
		key      types.NamespacedName
	)

	// reconcile runs one pass and re-reads the object, which is how the
	// controller actually behaves: each pass sees whatever the last one persisted.
	reconcile := func() (ctrl.Result, error) {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		_ = k8sClient.Get(ctx, key, machine)
		return res, err
	}

	BeforeEach(func() {
		provider = fake.New()
		r = &HydraMachineReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Provider: provider,
		}
		machine = newMachine(nil)
		key = client.ObjectKeyFromObject(machine)
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
	})

	AfterEach(func() {
		fetched := &infrav1.HydraMachine{}
		if err := k8sClient.Get(ctx, key, fetched); err == nil {
			if controllerutil.ContainsFinalizer(fetched, MachineFinalizer) {
				patch := client.MergeFrom(fetched.DeepCopy())
				controllerutil.RemoveFinalizer(fetched, MachineFinalizer)
				_ = k8sClient.Patch(ctx, fetched, patch)
			}
			_ = k8sClient.Delete(ctx, fetched)
		}
	})

	Context("creating", func() {
		It("creates exactly one machine and records its providerID", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.Count()).To(Equal(1))
			Expect(machine.Spec.ProviderID).NotTo(BeNil())
			Expect(*machine.Spec.ProviderID).To(HavePrefix("hydra://fake/"))
		})

		It("adds the finalizer before any infrastructure exists", func() {
			// If the finalizer were added after Create, a delete landing in that
			// window would remove the object and strand the machine.
			provider.CreateErr = errors.New("hypervisor unreachable")

			_, err := reconcile()
			Expect(err).To(HaveOccurred())
			Expect(provider.Count()).To(Equal(0))
			Expect(controllerutil.ContainsFinalizer(machine, MachineFinalizer)).To(BeTrue())
		})

		It("surfaces addresses and provisioning state", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			Expect(machine.Status.Addresses).NotTo(BeEmpty())
			Expect(machine.Status.Initialization.Provisioned).NotTo(BeNil())
			Expect(*machine.Status.Initialization.Provisioned).To(BeFalse())

			cond := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineReadyCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Provisioning"))
		})

		It("reports Ready once the backend says the machine is running", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			_, id, err := providers.ParseProviderID(*machine.Spec.ProviderID)
			Expect(err).NotTo(HaveOccurred())
			provider.SetReady(id, true, providers.Address{
				Type: providers.AddressTypeInternalIP, Address: "192.168.15.42",
			})

			res, err := reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueWhileHealthy),
				"a ready machine is re-checked slowly so a later vanished VM is observed")

			Expect(*machine.Status.Initialization.Provisioned).To(BeTrue())
			cond := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineReadyCondition)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.ObservedGeneration).To(Equal(machine.Generation))
			Expect(machine.Status.Addresses[0].Address).To(Equal("192.168.15.42"))
		})

		It("polls while the machine is still provisioning", func() {
			res, err := reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		})
	})

	Context("repeated reconciles", func() {
		It("does not create a second machine", func() {
			for range 3 {
				_, err := reconcile()
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(provider.Count()).To(Equal(1), "exactly one machine per HydraMachine")
		})

		It("adopts an orphaned machine instead of creating a second one", func() {
			// Simulates a crash between creating the machine and persisting its
			// providerID: the machine exists on the backend, but the object has no
			// record of it. Note providerID cannot simply be cleared to reproduce
			// this -- the API rejects that, since it is immutable once set -- so the
			// orphan is created directly, which is what the real failure leaves
			// behind anyway.
			orphan, err := provider.Create(ctx, providers.MachineSpec{Name: backendName(machine)})
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(Equal(1))

			_, err = reconcile()
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.Count()).To(Equal(1), "the orphan must be adopted, not duplicated")
			Expect(provider.CreateCalls).To(Equal(2), "Create ran again and was idempotent")
			Expect(*machine.Spec.ProviderID).To(HaveSuffix(orphan.ID),
				"the object must adopt the existing machine's ID")
		})
	})

	Context("failures", func() {
		It("reports a retryable failure without raising the terminal condition", func() {
			// An unreachable hypervisor will probably work next time. Marking it
			// terminal invites a MachineHealthCheck to remediate a machine that was
			// about to be fine.
			provider.CreateErr = errors.New("hypervisor unreachable")

			_, err := reconcile()
			Expect(err).To(HaveOccurred())

			ready := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineReadyCondition)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("ProvisioningFailedRetrying"))
			Expect(ready.Message).To(ContainSubstring("hypervisor unreachable"))

			Expect(apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineProvisioningFailedCondition)).
				To(BeNil(), "a retryable error must not raise the terminal condition")
		})

		It("raises the terminal condition for a failure that will not fix itself", func() {
			provider.CreateErr = fmt.Errorf("%w: image not found in pool", providers.ErrTerminal)

			_, err := reconcile()
			Expect(err).To(HaveOccurred())

			failed := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineProvisioningFailedCondition)
			Expect(failed).NotTo(BeNil())
			Expect(failed.Status).To(Equal(metav1.ConditionTrue))
			Expect(failed.Message).To(ContainSubstring("image not found in pool"))
		})

		It("does not recreate a machine that vanished", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			// The VM disappeared. Recreating is wrong: providerID is immutable, and a
			// new machine would carry a different ID, so the Node association would
			// silently be against the wrong machine.
			provider.GetErr = providers.ErrNotFound

			_, err = reconcile()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be replaced"))
			Expect(provider.CreateCalls).To(Equal(1), "no attempt to recreate")

			failed := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineProvisioningFailedCondition)
			Expect(failed).NotTo(BeNil(), "vanishing is terminal: the object cannot be repaired by retrying")
			Expect(failed.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("identity and safety", func() {
		It("derives a globally unique backend name", func() {
			// A libvirt domain namespace is flat; a Kubernetes name is not unique
			// across namespaces. Two machines with the same name must not collide.
			name := backendName(machine)
			Expect(name).To(HavePrefix(machine.Namespace + "-" + machine.Name + "-"))
			Expect(name).NotTo(Equal(machine.Name), "the object name alone is not globally unique")

			other := machine.DeepCopy()
			other.Namespace = "other-namespace"
			other.UID = "ffffffff-0000-0000-0000-000000000000"
			Expect(backendName(other)).NotTo(Equal(name))
		})

		It("refuses a providerID belonging to another backend", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			// Simulate the controller being pointed at a different backend than the
			// one that created this machine.
			r.Provider = &renamedProvider{Provider: provider, name: "proxmox"}

			_, err = reconcile()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("belongs to backend"))
			Expect(provider.CreateCalls).To(Equal(1), "must not touch the other backend's machine")
		})

		It("latches provisioned once true, even if the machine later stops", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			_, id, err := providers.ParseProviderID(*machine.Spec.ProviderID)
			Expect(err).NotTo(HaveOccurred())
			provider.SetReady(id, true, providers.Address{Type: providers.AddressTypeInternalIP, Address: "10.0.0.5"})
			_, err = reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(*machine.Status.Initialization.Provisioned).To(BeTrue())

			// The VM stops. provisioned is an initialization milestone, so it must
			// not regress -- that would confuse Cluster API's orchestration. Ready
			// carries ongoing health instead.
			provider.SetReady(id, false)
			_, err = reconcile()
			Expect(err).NotTo(HaveOccurred())

			Expect(*machine.Status.Initialization.Provisioned).To(BeTrue(), "provisioned must latch")
			ready := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineReadyCondition)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse), "Ready should reflect that it stopped")
		})

		It("keeps polling a running machine until an address appears", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())
			_, id, err := providers.ParseProviderID(*machine.Spec.ProviderID)
			Expect(err).NotTo(HaveOccurred())

			// Running, but only a hostname so far -- which says nothing about
			// reachability. Addresses arrive later and nothing else triggers a
			// refresh, so the controller must keep looking.
			provider.SetReady(id, true, providers.Address{
				Type: providers.AddressTypeHostname, Address: "worker",
			})
			res, err := reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0), "must keep polling for an address")

			provider.SetReady(id, true, providers.Address{
				Type: providers.AddressTypeInternalIP, Address: "10.0.0.5",
			})
			res, err = reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueWhileHealthy),
				"an addressed, ready machine is re-checked on a health interval")
		})
	})

	Context("pausing", func() {
		pause := func() {
			patch := client.MergeFrom(machine.DeepCopy())
			if machine.Annotations == nil {
				machine.Annotations = map[string]string{}
			}
			machine.Annotations[clusterv1.PausedAnnotation] = "true"
			Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())
		}

		It("creates nothing while paused, and reports why", func() {
			pause()

			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.Count()).To(Equal(0), "a paused machine must not be provisioned")
			Expect(machine.Spec.ProviderID).To(BeNil())

			cond := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachinePausedCondition)
			Expect(cond).NotTo(BeNil(), "the contract asks for a Paused condition")
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("resumes when the annotation is removed", func() {
			pause()
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(Equal(0))

			patch := client.MergeFrom(machine.DeepCopy())
			delete(machine.Annotations, clusterv1.PausedAnnotation)
			Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())

			_, err = reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(Equal(1))

			cond := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachinePausedCondition)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		})

		It("does not destroy infrastructure while paused", func() {
			// An operator pausing a machine to investigate it would not expect the
			// controller to keep tearing infrastructure down underneath them.
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(Equal(1))

			pause()
			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.Count()).To(Equal(1), "deletion must wait for the pause to be lifted")
			Expect(k8sClient.Get(ctx, key, machine)).To(Succeed(), "the finalizer must hold the object")
		})
	})

	Context("deleting", func() {
		It("deletes a machine whose providerID was never recorded", func() {
			// The crash window: Create succeeded, the providerID patch did not.
			// Without a name fallback this releases the finalizer and orphans a
			// running VM permanently.
			orphan, err := provider.Create(ctx, providers.MachineSpec{Name: backendName(machine)})
			Expect(err).NotTo(HaveOccurred())
			Expect(orphan).NotTo(BeNil())
			Expect(provider.Count()).To(Equal(1))

			patch := client.MergeFrom(machine.DeepCopy())
			controllerutil.AddFinalizer(machine, MachineFinalizer)
			Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())
			Expect(machine.Spec.ProviderID).To(BeNil(), "precondition: no providerID recorded")

			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.Count()).To(Equal(0), "the unrecorded machine must still be removed")
		})

		It("removes the machine and releases the finalizer", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(Equal(1))

			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.Count()).To(Equal(0), "provider-owned resources must be gone")
			err = k8sClient.Get(ctx, key, machine)
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			Expect(err).To(HaveOccurred(), "object should be gone once the finalizer is released")
		})

		It("keeps the finalizer when deletion fails", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			provider.DeleteErr = errors.New("hypervisor unreachable")
			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())

			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, machine)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(machine, MachineFinalizer)).To(BeTrue(),
				"a failed delete must not release the object and strand the machine")
		})

		It("removes leftover storage when Create never defined a domain", func() {
			// Create can allocate a volume and then fail before DomainDefineXML.
			// FindByName only sees domains, so without DeleteByName this would
			// release the finalizer and leave the qcow2 behind forever.
			provider.AddPartial(backendName(machine))
			Expect(provider.PartialCount()).To(Equal(1))

			patch := client.MergeFrom(machine.DeepCopy())
			controllerutil.AddFinalizer(machine, MachineFinalizer)
			Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())
			Expect(machine.Spec.ProviderID).To(BeNil(), "precondition: no providerID recorded")

			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.PartialCount()).To(Equal(0), "the leftover volume must still be removed")
		})

		It("succeeds when the machine is already gone", func() {
			_, err := reconcile()
			Expect(err).NotTo(HaveOccurred())

			_, id, err := providers.ParseProviderID(*machine.Spec.ProviderID)
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Delete(ctx, id)).To(Succeed())

			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred(), "deleting an absent machine is the desired end state")
		})
	})
})
