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
	"maps"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers/fake"
)

// otherObjectUID stands for an object that is not the one under test: the
// source of a move, or whatever a stray annotation names.
const otherObjectUID = "0b9c1a64-1111-4222-8333-444455556666"

// These specs cover a HydraMachine moving between management clusters with
// `clusterctl move` (PET-76). One API server cannot hold the source and target
// copies at once, so a move is played the way clusterctl leaves it: the source
// object removed without teardown -- clusterctl strips its finalizers -- and an
// object of the same namespace and name created with the same spec and
// annotations, which the API server gives a new UID.
var _ = Describe("HydraMachine Reconciler — moving between management clusters", func() {
	ctx := context.Background()

	var (
		provider *fake.Provider
		r        *HydraMachineReconciler
		machine  *infrav1.HydraMachine
		key      types.NamespacedName
	)

	reconcile := func() (ctrl.Result, error) {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		_ = k8sClient.Get(ctx, key, machine)
		return res, err
	}

	// gone removes an object the way clusterctl removes a moved one from its
	// source: finalizer stripped, so no teardown runs.
	gone := func(m *infrav1.HydraMachine) {
		fetched := &infrav1.HydraMachine{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(m), fetched); err != nil {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			return
		}
		patch := client.MergeFrom(fetched.DeepCopy())
		fetched.Finalizers = nil
		Expect(k8sClient.Patch(ctx, fetched, patch)).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, fetched))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(m), &infrav1.HydraMachine{}))
		}).Should(BeTrue())
	}

	// copyOf is what clusterctl creates on the target: the same namespace,
	// name, annotations and spec, providerID included. The API server assigns
	// the UID, so it is a new one.
	copyOf := func(src *infrav1.HydraMachine) *infrav1.HydraMachine {
		annotations := maps.Clone(src.Annotations)
		return &infrav1.HydraMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:        src.Name,
				Namespace:   src.Namespace,
				Annotations: annotations,
				Finalizers:  []string{MachineFinalizer},
			},
			Spec: *src.Spec.DeepCopy(),
		}
	}

	// move replaces machine with a moved copy of itself and returns the old
	// UID, which the copy's identity annotation still names.
	move := func() types.UID {
		old := machine.DeepCopy()
		gone(machine)
		moved := copyOf(old)
		Expect(k8sClient.Create(ctx, moved)).To(Succeed())
		Expect(moved.UID).NotTo(Equal(old.UID), "precondition: a moved object gets a new UID")
		machine = moved
		return old.UID
	}

	// machineID is the backend ID behind the object's providerID.
	machineID := func() string {
		Expect(machine.Spec.ProviderID).NotTo(BeNil())
		_, id, err := providers.ParseProviderID(*machine.Spec.ProviderID)
		Expect(err).NotTo(HaveOccurred())
		return id
	}

	ownerOf := func(id string) string {
		state, err := provider.Get(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		return state.Owner
	}

	deleteAndReconcile := func() error {
		Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		return err
	}

	BeforeEach(func() {
		provider = fake.New()
		r = &HydraMachineReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			Provider:  provider,
		}
		machine = newMachine(func(m *infrav1.HydraMachine) {
			m.Annotations = map[string]string{infrav1.StandaloneAnnotation: ""}
		})
		key = client.ObjectKeyFromObject(machine)
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
	})

	AfterEach(func() {
		gone(machine)
	})

	It("records its identity and its ownership when it creates a machine", func() {
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())

		Expect(machine.Annotations).To(HaveKeyWithValue(infrav1.IdentityUIDAnnotation, string(machine.UID)))
		Expect(provider.LastSpec.OwnerUID).To(Equal(string(machine.UID)))
		Expect(ownerOf(machineID())).To(Equal(string(machine.UID)))
	})

	It("records both on a machine made before they existed, so it can be moved", func() {
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		id := machineID()

		// What a machine made by an earlier release looks like.
		provider.SetOwner(id, "")
		patch := client.MergeFrom(machine.DeepCopy())
		delete(machine.Annotations, infrav1.IdentityUIDAnnotation)
		Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())

		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(ownerOf(id)).To(Equal(string(machine.UID)))
		Expect(machine.Annotations).To(HaveKeyWithValue(infrav1.IdentityUIDAnnotation, string(machine.UID)))
	})

	It("takes over the machine it was moved with, and can then delete it", func() {
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		id := machineID()

		oldUID := move()
		Expect(ownerOf(id)).To(Equal(string(oldUID)), "precondition: still recorded as the source object's")

		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(ownerOf(id)).To(Equal(string(machine.UID)), "the moved object must take the machine over")
		Expect(machine.Annotations).To(HaveKeyWithValue(infrav1.IdentityUIDAnnotation, string(oldUID)),
			"the identity stays the UID the name was derived from")
		Expect(provider.Count()).To(Equal(1), "nothing recreated")

		Expect(deleteAndReconcile()).To(Succeed())
		Expect(provider.Count()).To(Equal(0), "the machine is deleted with the moved object")
	})

	It("takes over again after a second move", func() {
		// A pivot back, or a chain of management clusters. After the first
		// takeover the machine records the first copy as its owner, which only
		// the owner-uid annotation that copy wrote can tell the second.
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		id := machineID()

		move()
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		first := machine.UID
		Expect(machine.Annotations).To(HaveKeyWithValue(infrav1.OwnerUIDAnnotation, string(first)))

		move()
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(ownerOf(id)).To(Equal(string(machine.UID)))
		Expect(ownerOf(id)).NotTo(Equal(string(first)))

		Expect(deleteAndReconcile()).To(Succeed())
		Expect(provider.Count()).To(Equal(0))
	})

	It("takes over a machine moved before its owner was ever recorded", func() {
		// Create's window: the identity is written before Create, the owner
		// record by Create itself, and a failed write leaves the machine owned
		// by nobody. A move landing then must not strand it.
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		id := machineID()
		provider.SetOwner(id, "")
		patch := client.MergeFrom(machine.DeepCopy())
		delete(machine.Annotations, infrav1.OwnerUIDAnnotation)
		Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())

		move()
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())
		Expect(ownerOf(id)).To(Equal(string(machine.UID)))
	})

	It("sweeps the moved-from name once it has taken the machine over", func() {
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		id := machineID()
		oldUID := move()
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())

		// The domain is undefined behind the provider's back; its disks are not.
		Expect(provider.Delete(ctx, id)).To(Succeed())
		provider.AddPartial(backendNameFor(machine, string(oldUID)))

		Expect(deleteAndReconcile()).To(Succeed())
		Expect(provider.PartialCount()).To(Equal(0), "what it owned must not be left behind")
	})

	It("refuses a stale copy once the move has been taken over, and deletes nothing for it", func() {
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		id := machineID()
		original := machine.DeepCopy()

		move()
		_, err = reconcile()
		Expect(err).NotTo(HaveOccurred())

		// A second copy of the original appears -- a re-applied manifest, a
		// restored backup -- naming the same identity and providerID.
		gone(machine)
		stale := copyOf(original)
		Expect(k8sClient.Create(ctx, stale)).To(Succeed())
		machine = stale

		_, err = reconcile()
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("recorded as owned by"))

		Expect(deleteAndReconcile()).To(HaveOccurred())
		Expect(provider.Count()).To(Equal(1), "the machine another object owns must survive")
		Expect(ownerOf(id)).NotTo(Equal(string(stale.UID)))
	})

	It("will not delete a moved machine it never took over", func() {
		// The failed move: copies exist on the target, still paused, and an
		// operator deletes them while the source still runs the machine.
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())
		id := machineID()
		oldUID := move()

		err = deleteAndReconcile()
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("has not been taken over"))
		Expect(provider.Count()).To(Equal(1), "the source's machine must survive")
		Expect(ownerOf(id)).To(Equal(string(oldUID)))
	})

	It("refuses another object's identity on a machine with no providerID", func() {
		// A template's metadata, or a hand edit: nothing explains it safely.
		patch := client.MergeFrom(machine.DeepCopy())
		machine.Annotations[infrav1.IdentityUIDAnnotation] = otherObjectUID
		Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())

		_, err := reconcile()
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue(), "got %v", err)
		Expect(provider.CreateCalls).To(Equal(0), "nothing may be built under a borrowed identity")
	})

	It("refuses another object's owner record on a machine with no providerID", func() {
		patch := client.MergeFrom(machine.DeepCopy())
		machine.Annotations[infrav1.OwnerUIDAnnotation] = otherObjectUID
		Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())

		_, err := reconcile()
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue(), "got %v", err)
		Expect(provider.CreateCalls).To(Equal(0))
	})

	It("sweeps only its own name when deleted with another object's identity and no providerID", func() {
		theirs, err := provider.Create(ctx, providers.MachineSpec{
			Name:     backendNameFor(machine, otherObjectUID),
			OwnerUID: otherObjectUID,
		})
		Expect(err).NotTo(HaveOccurred())

		patch := client.MergeFrom(machine.DeepCopy())
		machine.Annotations[infrav1.IdentityUIDAnnotation] = otherObjectUID
		controllerutil.AddFinalizer(machine, MachineFinalizer)
		Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())

		Expect(deleteAndReconcile()).To(Succeed())
		_, err = provider.Get(ctx, theirs.ID)
		Expect(err).NotTo(HaveOccurred(), "the machine named for the other identity must survive")
	})

	It("refuses an identity that is not a UID, before deriving any name from it", func() {
		// On a machine that has one, so no other guard -- the foreign identity
		// without a providerID, the name check -- can refuse it first. A value
		// with a slash in it must never reach the namespace/name/uid hash.
		_, err := reconcile()
		Expect(err).NotTo(HaveOccurred())

		patch := client.MergeFrom(machine.DeepCopy())
		machine.Annotations[infrav1.IdentityUIDAnnotation] = "a/b"
		Expect(k8sClient.Patch(ctx, machine, patch)).To(Succeed())

		_, err = reconcile()
		Expect(errors.Is(err, providers.ErrTerminal)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("not an object UID"))

		Expect(deleteAndReconcile()).To(HaveOccurred(), "teardown must not guess a name either")
		Expect(provider.Count()).To(Equal(1))
	})

	It("will not adopt a machine under its own name that another object owns", func() {
		_, err := provider.Create(ctx, providers.MachineSpec{
			Name:     backendName(machine),
			OwnerUID: otherObjectUID,
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = reconcile()
		Expect(errors.Is(err, providers.ErrOwnerMismatch)).To(BeTrue(), "got %v", err)
		Expect(machine.Spec.ProviderID).To(BeNil(), "it must not record another object's machine as its own")
	})
})
