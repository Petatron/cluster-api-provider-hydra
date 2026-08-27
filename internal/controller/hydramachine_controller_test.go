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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers/fake"
)

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
			Expect(res.RequeueAfter).To(BeZero(), "a ready machine should not be re-polled")

			Expect(*machine.Status.Initialization.Provisioned).To(BeTrue())
			cond := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineReadyCondition)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
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
			orphan, err := provider.Create(ctx, providers.MachineSpec{Name: machine.Name})
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
		It("reports a creation failure through conditions", func() {
			provider.CreateErr = errors.New("no capacity on host")

			_, err := reconcile()
			Expect(err).To(HaveOccurred())

			failed := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineProvisioningFailedCondition)
			Expect(failed).NotTo(BeNil())
			Expect(failed.Status).To(Equal(metav1.ConditionTrue))
			Expect(failed.Message).To(ContainSubstring("no capacity on host"))

			ready := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineReadyCondition)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
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
		})
	})

	Context("deleting", func() {
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
