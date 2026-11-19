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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

const testHookPath = "/usr/local/sbin/hydra-pre-drain"

// The pre-drain hook decision table (predrain.go), one row per case.
var _ = Describe("Pre-drain guest hook", func() {
	ctx := context.Background()

	var (
		provider *fake.Provider
		hm       *infrav1.HydraMachine
		key      types.NamespacedName
		secret   = linkSecretName
	)

	// provisioned builds a reconciler whose machine already runs in the fake
	// backend, owned by capi; extra objects are added alongside.
	provisioned := func(capi *clusterv1.Machine, extra ...client.Object) *HydraMachineReconciler {
		s := linkScheme()
		objs := append([]client.Object{hm, capi, bootstrapSecret(secret, map[string][]byte{bootstrapDataSecretKey: []byte("#cloud-config\n")})}, extra...)
		hasCluster := false
		for _, o := range extra {
			if _, ok := o.(*clusterv1.Cluster); ok {
				hasCluster = true
			}
		}
		if !hasCluster {
			objs = append(objs, readyCluster())
		}
		c := fakeclient.NewClientBuilder().WithScheme(s).WithObjects(objs...).
			WithStatusSubresource(&infrav1.HydraMachine{}).Build()
		r := &HydraMachineReconciler{Client: c, APIReader: c, Scheme: s, Provider: provider}
		// Provision it with the Machine not yet deleting, as it would have been.
		live := &clusterv1.Machine{}
		Expect(c.Get(ctx, client.ObjectKeyFromObject(capi), live)).To(Succeed())
		if live.DeletionTimestamp.IsZero() {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		return r
	}

	machineOf := func(r *HydraMachineReconciler) *clusterv1.Machine {
		m := &clusterv1.Machine{}
		Expect(r.Get(ctx, types.NamespacedName{Namespace: linkNamespace, Name: "capi-" + linkMachineName}, m)).To(Succeed())
		return m
	}

	held := func(r *HydraMachineReconciler) bool {
		_, ok := machineOf(r).Annotations[infrav1.PreDrainHookAnnotation]
		return ok
	}

	// deletingOwner is the owner Machine, deleting since `ago`, holding the hook.
	deletingOwner := func(ago time.Duration) *clusterv1.Machine {
		m := ownerMachine(&secret)
		m.Annotations = map[string]string{infrav1.PreDrainHookAnnotation: infrav1.PreDrainHookOwner}
		m.Finalizers = []string{"machine.cluster.x-k8s.io"}
		m.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-ago)}
		return m
	}

	// running provisions hm in the fake backend, ready, as if done earlier.
	running := func() {
		provider.ReadyOnCreate = true
		pre := provisioned(ownerMachine(&secret))
		Expect(provider.Count()).To(Equal(1))
		hm = reloadHM(ctx, pre, key)
		hm.ResourceVersion = ""
	}

	// runRecord is the run annotation for the deleting owner's deletion.
	runRecord := func(r *HydraMachineReconciler, rest string) string {
		m := machineOf(r)
		return fmt.Sprintf("%s/%d/%s", m.UID, m.DeletionTimestamp.Unix(), rest)
	}

	// withRun stores a run record on the HydraMachine.
	withRun := func(r *HydraMachineReconciler, value string) {
		cur := reloadHM(ctx, r, key)
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		cur.Annotations[PreDrainHookRunAnnotation] = value
		Expect(r.Update(ctx, cur)).To(Succeed())
	}

	reconcile := func(r *HydraMachineReconciler) (ctrl.Result, error) {
		return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}

	BeforeEach(func() {
		provider = fake.New()
		hm = linkedMachine(linkMachineName)
		hm.Spec.PreDrainHook = &infrav1.HydraGuestHook{Path: testHookPath}
		key = client.ObjectKeyFromObject(hm)
	})

	It("holds the Machine's deletion with a pre-drain hook when preDrainHook is set", func() {
		r := provisioned(ownerMachine(&secret))
		Expect(machineOf(r).Annotations).To(HaveKeyWithValue(infrav1.PreDrainHookAnnotation, infrav1.PreDrainHookOwner))
	})

	It("removes a hook nobody would serve when preDrainHook is unset", func() {
		hm.Spec.PreDrainHook = nil
		m := ownerMachine(&secret)
		m.Annotations = map[string]string{infrav1.PreDrainHookAnnotation: infrav1.PreDrainHookOwner}
		r := provisioned(m)
		Expect(held(r)).To(BeFalse())
	})

	It("removes a leftover run record from a Machine that is not deleting", func() {
		running()
		hm.Annotations = map[string]string{PreDrainHookRunAnnotation: "old-uid/1/42"}
		r := provisioned(ownerMachine(&secret))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(reloadHM(ctx, r, key).Annotations).NotTo(HaveKey(PreDrainHookRunAnnotation))
	})

	It("releases the hook when the HydraMachine is deleted first, so the Machine is never stuck", func() {
		running()
		r := provisioned(ownerMachine(&secret))
		Expect(held(r)).To(BeTrue())
		deletingHM := reloadHM(ctx, r, key)
		deletingHM.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		_, err := r.reconcilePreDrainHook(ctx, deletingHM, &linkage{machine: machineOf(r)}, provider)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
	})

	It("never adds the hook while the HydraMachine itself is deleting", func() {
		running()
		r := provisioned(ownerMachine(&secret))
		m := machineOf(r)
		delete(m.Annotations, infrav1.PreDrainHookAnnotation)
		Expect(r.Update(ctx, m)).To(Succeed())
		deletingHM := reloadHM(ctx, r, key)
		deletingHM.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		_, err := r.reconcilePreDrainHook(ctx, deletingHM, &linkage{machine: machineOf(r)}, provider)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
	})

	It("never runs a hook Cluster API skipped: both the Machine and the HydraMachine are deleting", func() {
		running()
		r := provisioned(deletingOwner(time.Second))
		deletingHM := reloadHM(ctx, r, key)
		deletingHM.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		requeue, err := r.reconcilePreDrainHook(ctx, deletingHM, &linkage{machine: machineOf(r)}, provider)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeZero())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("never adds the hook to a Machine already deleting", func() {
		running()
		m := deletingOwner(time.Second)
		m.Annotations = nil
		r := provisioned(m)
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("releases a held hook without running it when preDrainHook is unset on a deleting Machine", func() {
		running()
		hm.Spec.PreDrainHook = nil
		r := provisioned(deletingOwner(time.Second))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("starts the program once, polls it, and releases the hook when it exits 0", func() {
		running()
		provider.GuestRunning = true
		r := provisioned(deletingOwner(time.Second))

		res, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(preDrainHookPoll))
		Expect(provider.GuestStarts).To(Equal([]string{testHookPath}))
		Expect(reloadHM(ctx, r, key).Annotations).To(HaveKeyWithValue(PreDrainHookRunAnnotation, runRecord(r, "1")))
		Expect(held(r)).To(BeTrue())

		// Still running: poll, never start a second copy.
		res, err = reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(preDrainHookPoll))
		Expect(provider.GuestStarts).To(HaveLen(1))
		Expect(held(r)).To(BeTrue())

		provider.GuestRunning = false
		provider.GuestResult = providers.GuestRun{ExitCode: 0, Output: "handed over"}
		_, err = reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(HaveLen(1))
	})

	It("releases the hook when the program fails: a failed hook never blocks the deletion", func() {
		running()
		r := provisioned(deletingOwner(time.Second))
		provider.GuestResult = providers.GuestRun{ExitCode: 1, Output: "lease still here"}
		_, err := reconcile(r) // start
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcile(r) // exited 1
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
	})

	It("releases the hook when the program runs past its timeout", func() {
		running()
		provider.GuestRunning = true
		r := provisioned(deletingOwner(infrav1.DefaultGuestHookTimeoutSeconds*time.Second + 5*time.Second))
		withRun(r, runRecord(r, "7"))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("never starts the program after its timeout: too late, it would race the drain", func() {
		running()
		r := provisioned(deletingOwner(infrav1.DefaultGuestHookTimeoutSeconds*time.Second + 5*time.Second))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("releases the hook unrun once timeout and slack have passed", func() {
		running()
		r := provisioned(deletingOwner(infrav1.DefaultGuestHookTimeoutSeconds*time.Second + preDrainHookSlack + time.Second))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("retries a start that failed by requeueing, keeping the hook, and lets a later start through", func() {
		running()
		provider.GuestStartErr = errors.New("guest agent is not responding")
		r := provisioned(deletingOwner(time.Second))
		res, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred(), "a failed start requeues; an error would back off past the bound")
		Expect(res.RequeueAfter).To(Equal(preDrainHookRetry))
		Expect(held(r)).To(BeTrue())
		Expect(reloadHM(ctx, r, key).Annotations).NotTo(HaveKey(PreDrainHookRunAnnotation))

		provider.GuestStartErr = nil
		_, err = reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.GuestStarts).To(HaveLen(2))
		Expect(reloadHM(ctx, r, key).Annotations).To(HaveKeyWithValue(PreDrainHookRunAnnotation, runRecord(r, "2")))
	})

	It("releases the hook when the guest refuses to run the program", func() {
		running()
		provider.GuestStartErr = fmt.Errorf("%w: no such file", providers.ErrGuestRefused)
		r := provisioned(deletingOwner(time.Second))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
	})

	It("releases the hook when the guest no longer knows the run", func() {
		running()
		provider.GuestStatusErr = fmt.Errorf("%w: invalid pid", providers.ErrGuestRefused)
		r := provisioned(deletingOwner(time.Second))
		withRun(r, runRecord(r, "9"))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("retries a status read that failed by requeueing", func() {
		running()
		provider.GuestStatusErr = errors.New("connection reset")
		r := provisioned(deletingOwner(time.Second))
		withRun(r, runRecord(r, "9"))
		res, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(preDrainHookRetry))
		Expect(held(r)).To(BeTrue())
	})

	It("holds, without starting again, a start whose outcome is unknown, until its timeout", func() {
		running()
		r := provisioned(deletingOwner(time.Second))
		withRun(r, runRecord(r, runStarting))
		res, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(held(r)).To(BeTrue(), "the program may be running: the drain waits")
		Expect(provider.GuestStarts).To(BeEmpty(), "never a second copy")
	})

	It("releases a start whose outcome is unknown once its timeout has passed", func() {
		running()
		r := provisioned(deletingOwner(infrav1.DefaultGuestHookTimeoutSeconds*time.Second + 5*time.Second))
		withRun(r, runRecord(r, runStarting))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("treats a start that reports no pid as a failed start, and retries it", func() {
		running()
		provider.GuestPID = -1
		r := provisioned(deletingOwner(time.Second))
		res, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(preDrainHookRetry))
		Expect(held(r)).To(BeTrue())
		Expect(reloadHM(ctx, r, key).Annotations).NotTo(HaveKey(PreDrainHookRunAnnotation))
	})

	It("ignores a run record from another deletion, such as one copied by clusterctl move", func() {
		running()
		provider.GuestRunning = true
		r := provisioned(deletingOwner(time.Second))
		withRun(r, "some-other-uid/12345/77")
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.GuestStarts).To(HaveLen(1))
		Expect(reloadHM(ctx, r, key).Annotations).To(HaveKeyWithValue(PreDrainHookRunAnnotation, runRecord(r, "1")))
	})

	It("releases the hook without running it when the machine is not running", func() {
		provider.ReadyOnCreate = false
		pre := provisioned(ownerMachine(&secret))
		hm = reloadHM(ctx, pre, key)
		hm.ResourceVersion = ""
		r := provisioned(deletingOwner(time.Second))
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
	})

	It("releases the hook without running it while the Cluster is being deleted", func() {
		running()
		cluster := readyCluster()
		cluster.Finalizers = []string{"cluster.cluster.x-k8s.io"}
		cluster.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		r := provisioned(deletingOwner(time.Second), cluster)
		_, err := reconcile(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
		Expect(provider.GuestStarts).To(BeEmpty())
	})

	It("releases an expired hook even when no provider is available", func() {
		running()
		r := provisioned(deletingOwner(infrav1.DefaultGuestHookTimeoutSeconds*time.Second + preDrainHookSlack + time.Second))
		link := &linkage{machine: machineOf(r)}
		_, err := r.reconcilePreDrainHook(ctx, reloadHM(ctx, r, key), link, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(held(r)).To(BeFalse())
	})

	It("keeps a hook that has time left when no provider is available, on a timer within the bound", func() {
		running()
		r := provisioned(deletingOwner(time.Second))
		link := &linkage{machine: machineOf(r)}
		requeue, err := r.reconcilePreDrainHook(ctx, reloadHM(ctx, r, key), link, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(Equal(preDrainHookRetry), "a timer, not the caller's growing error backoff")
		Expect(held(r)).To(BeTrue())
	})

	It("never waits past the bound for a missing provider", func() {
		running()
		r := provisioned(deletingOwner(infrav1.DefaultGuestHookTimeoutSeconds*time.Second + preDrainHookSlack - 2*time.Second))
		link := &linkage{machine: machineOf(r)}
		requeue, err := r.reconcilePreDrainHook(ctx, reloadHM(ctx, r, key), link, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeNumerically("<=", 4*time.Second))
	})
})

func reloadHM(ctx context.Context, r *HydraMachineReconciler, key types.NamespacedName) *infrav1.HydraMachine {
	out := &infrav1.HydraMachine{}
	Expect(r.Get(ctx, key, out)).To(Succeed())
	return out
}
