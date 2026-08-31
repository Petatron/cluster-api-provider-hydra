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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers/fake"
)

// These specs use a fake client rather than the envtest environment the rest of
// the suite runs against, because they need Machine and Cluster objects to
// exist. Cluster API's CRDs are not vendored into this repository -- only its Go
// API module is -- so envtest has no schema to admit them under. The behaviour
// under test is the controller's reading of those objects, which a fake client
// reproduces exactly.

const (
	linkNamespace   = "default"
	linkClusterName = "hydra"
	linkMachineName = "worker-1"
	linkSecretName  = "worker-1-bootstrap"
)

func linkScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(s)).To(Succeed())
	Expect(clusterv1.AddToScheme(s)).To(Succeed())
	Expect(infrav1.AddToScheme(s)).To(Succeed())
	return s
}

// linkedMachine builds a HydraMachine owned by a Cluster API Machine, which is
// how every machine a MachineDeployment creates actually arrives.
func linkedMachine(name string) *infrav1.HydraMachine {
	return &infrav1.HydraMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: linkNamespace,
			UID:       types.UID("uid-" + name),
			Labels:    map[string]string{clusterv1.ClusterNameLabel: linkClusterName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       machineKind,
				Name:       "capi-" + name,
				UID:        types.UID("uid-capi-" + name),
			}},
		},
		Spec: infrav1.HydraMachineSpec{
			VCPUs:    2,
			Memory:   resource.MustParse("4Gi"),
			DiskSize: resource.MustParse("40Gi"),
			Image:    infrav1.HydraMachineImage{Name: testImage},
			Networks: []infrav1.HydraMachineNetworkAttachment{{Name: testNetwork}},
		},
	}
}

func ownerMachine(dataSecret *string) *clusterv1.Machine {
	name := linkMachineName
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "capi-" + name,
			Namespace: linkNamespace,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: linkClusterName},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: linkClusterName,
			Bootstrap:   clusterv1.Bootstrap{DataSecretName: dataSecret},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupVersion.Group,
				Kind:     hydraMachineKind,
				Name:     name,
			},
		},
	}
}

func bootstrapSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: linkNamespace},
		Data:       data,
	}
}

var _ = Describe("Cluster API linkage", func() {
	ctx := context.Background()

	var (
		provider *fake.Provider
		hm       *infrav1.HydraMachine
		key      types.NamespacedName
	)

	// build wires a reconciler over exactly the objects a spec cares about.
	build := func(objs ...client.Object) *HydraMachineReconciler {
		s := linkScheme()
		all := append([]client.Object{hm}, objs...)
		c := fakeclient.NewClientBuilder().
			WithScheme(s).
			WithObjects(all...).
			WithStatusSubresource(&infrav1.HydraMachine{}).
			Build()
		// In production APIReader is the manager's uncached reader. The fake
		// client has no cache, so it stands in for both.
		return &HydraMachineReconciler{Client: c, APIReader: c, Scheme: s, Provider: provider}
	}

	reload := func(r *HydraMachineReconciler) *infrav1.HydraMachine {
		out := &infrav1.HydraMachine{}
		Expect(r.Get(ctx, key, out)).To(Succeed())
		return out
	}

	BeforeEach(func() {
		provider = fake.New()
		hm = linkedMachine(linkMachineName)
		key = client.ObjectKeyFromObject(hm)
	})

	Context("bootstrap data", func() {
		It("passes the Secret's value through to the backend", func() {
			userData := []byte("#cloud-config\nruncmd:\n  - kubeadm join\n")
			secretName := linkSecretName
			r := build(
				ownerMachine(&secretName),
				bootstrapSecret(secretName, map[string][]byte{bootstrapDataSecretKey: userData}),
			)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(provider.Count()).To(Equal(1))
			Expect(provider.LastSpec.BootstrapData).To(Equal(userData))
		})

		It("gives the machine a hostname from the object, not the hashed backend name", func() {
			secretName := linkSecretName
			r := build(
				ownerMachine(&secretName),
				bootstrapSecret(secretName, map[string][]byte{bootstrapDataSecretKey: []byte("#cloud-config\n")}),
			)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Without this every machine cloned from one base image answers to the
			// same hostname and they collide as Nodes.
			Expect(provider.LastSpec.Hostname).To(Equal(linkMachineName))
			// The backend name still carries its uniqueness hash; the two must not
			// have been conflated.
			Expect(provider.LastSpec.Name).NotTo(Equal(provider.LastSpec.Hostname))
		})

		It("waits, without failing, until the bootstrap provider names a Secret", func() {
			r := build(ownerMachine(nil))

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueWhileProvisioning))

			// Creating a VM before its bootstrap data exists would produce a machine
			// that boots, looks healthy, and never joins anything.
			Expect(provider.Count()).To(BeZero())

			out := reload(r)
			ready := apimeta.FindStatusCondition(out.Status.Conditions, infrav1.MachineReadyCondition)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("WaitingForBootstrapData"))

			// Waiting on CABPK is not a provisioning failure, and that condition is
			// what a MachineHealthCheck may remediate on.
			Expect(apimeta.FindStatusCondition(out.Status.Conditions, infrav1.MachineProvisioningFailedCondition)).To(BeNil())
		})

		It("waits when the Secret is named but not yet visible", func() {
			secretName := "not-created-yet"
			r := build(ownerMachine(&secretName))

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueWhileProvisioning))
			Expect(provider.Count()).To(BeZero())

			ready := apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.MachineReadyCondition)
			Expect(ready.Reason).To(Equal("WaitingForBootstrapData"))
			// The operator has to be able to tell which Secret is missing.
			Expect(ready.Message).To(ContainSubstring(secretName))
		})

		It("fails terminally when the Secret exists but has the wrong shape", func() {
			secretName := linkSecretName
			r := build(
				ownerMachine(&secretName),
				bootstrapSecret(secretName, map[string][]byte{"user-data": []byte("wrong key")}),
			)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())
			Expect(provider.Count()).To(BeZero())

			// No retry can add the key, so this must not scroll past as a retryable
			// failure forever.
			failed := apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.MachineProvisioningFailedCondition)
			Expect(failed).NotTo(BeNil())
			Expect(failed.Status).To(Equal(metav1.ConditionTrue))
		})

		It("fails terminally when the Secret's value is empty", func() {
			secretName := linkSecretName
			r := build(
				ownerMachine(&secretName),
				bootstrapSecret(secretName, map[string][]byte{bootstrapDataSecretKey: {}}),
			)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).To(HaveOccurred())
			Expect(provider.Count()).To(BeZero())
		})

		It("does not re-require bootstrap data once the machine exists", func() {
			// The Secret is consumed at first boot. Re-requiring it would strand a
			// running, healthy machine the moment its Secret was rotated or cleaned
			// up -- and Cluster API does clean them up.
			providerID := providers.ProviderID(provider.Name(), "fake-1")
			hm.Spec.ProviderID = &providerID
			r := build(ownerMachine(nil))

			state, err := provider.Create(ctx, providers.MachineSpec{Name: backendName(hm)})
			Expect(err).NotTo(HaveOccurred())
			Expect(state.ID).To(Equal("fake-1"))

			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			ready := apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.MachineReadyCondition)
			Expect(ready.Reason).NotTo(Equal("WaitingForBootstrapData"))
		})

		It("provisions with no bootstrap data when nothing owns it", func() {
			// A HydraMachine created directly is the documented way to exercise the
			// infrastructure half on its own: it boots and never joins a cluster.
			hm.OwnerReferences = nil
			r := build()

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(Equal(1))
			Expect(provider.LastSpec.BootstrapData).To(BeEmpty())
		})

		It("waits, rather than provisioning, when the owning Machine is not visible", func() {
			// An owner reference whose target cannot be read is NOT the standalone
			// case. Provisioning through it would create a VM with no bootstrap
			// data, and once providerID is persisted the Secret is never read
			// again -- so that VM could never be repaired and would never join.
			r := build()

			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(requeueWhileProvisioning))
			Expect(provider.Count()).To(BeZero())

			ready := apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.MachineReadyCondition)
			Expect(ready.Reason).To(Equal("WaitingForOwnerMachine"))
			Expect(ready.Message).To(ContainSubstring("capi-" + linkMachineName))
		})

		It("still tears down when the owning Machine is already gone", func() {
			// The wait above must not reach deletion. A Machine is removed before
			// the infrastructure it described, so blocking here would wedge the
			// finalizer on every machine deleted in the normal order.
			hm.Finalizers = []string{MachineFinalizer}
			now := metav1.Now()
			hm.DeletionTimestamp = &now
			r := build()

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			out := &infrav1.HydraMachine{}
			err = r.Get(ctx, key, out)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"the finalizer should have been released and the object collected")
		})
	})

	Context("hostnames", func() {
		It("bounds a name Linux would refuse, keeping it unique", func() {
			// metadata.name allows 253 characters; Linux caps a hostname at 63.
			// Passing a long one through does not fail loudly -- cloud-init simply
			// does not set it, and every clone of the base image answers to the
			// same name, which is the collision Hostname exists to prevent.
			long := strings.Repeat("a", 200)

			one := hostnameFor(&infrav1.HydraMachine{ObjectMeta: metav1.ObjectMeta{
				Name: long, Namespace: linkNamespace, UID: "uid-one",
			}})
			two := hostnameFor(&infrav1.HydraMachine{ObjectMeta: metav1.ObjectMeta{
				Name: long, Namespace: linkNamespace, UID: "uid-two",
			}})

			Expect(len(one)).To(BeNumerically("<=", maxHostnameLength))
			// Truncation alone would trade one collision for another.
			Expect(one).NotTo(Equal(two))
		})

		It("leaves a name that already fits alone", func() {
			Expect(hostnameFor(hm)).To(Equal(linkMachineName))
		})

		It("never ends a truncated hostname with a hyphen", func() {
			// A label may not start or end with one, and a name cut mid-word can.
			name := strings.Repeat("a", 54) + strings.Repeat("-", 20)
			got := hostnameFor(&infrav1.HydraMachine{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: linkNamespace, UID: "uid",
			}})
			Expect(got).NotTo(HaveSuffix("-"))
			Expect(len(got)).To(BeNumerically("<=", maxHostnameLength))
		})
	})

	Context("pausing", func() {
		pausedCluster := func(paused bool) *clusterv1.Cluster {
			return &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: linkClusterName, Namespace: linkNamespace},
				Spec:       clusterv1.ClusterSpec{Paused: &paused},
			}
		}

		It("honours spec.paused on the owning Cluster", func() {
			secretName := linkSecretName
			r := build(
				ownerMachine(&secretName),
				bootstrapSecret(secretName, map[string][]byte{bootstrapDataSecretKey: []byte("#cloud-config\n")}),
				pausedCluster(true),
			)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// This is the whole point: an operator who paused the Cluster expects
			// nothing to be created underneath them.
			Expect(provider.Count()).To(BeZero())

			cond := apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.MachinePausedCondition)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			// Three things can pause a machine, so the message has to say which.
			Expect(cond.Message).To(ContainSubstring("spec.paused"))
		})

		It("honours the paused annotation on the owning Cluster", func() {
			cluster := pausedCluster(false)
			cluster.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
			r := build(ownerMachine(nil), cluster)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(BeZero())

			cond := apimeta.FindStatusCondition(reload(r).Status.Conditions, infrav1.MachinePausedCondition)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Message).To(ContainSubstring("Cluster"))
		})

		It("proceeds when the Cluster is not paused", func() {
			secretName := linkSecretName
			r := build(
				ownerMachine(&secretName),
				bootstrapSecret(secretName, map[string][]byte{bootstrapDataSecretKey: []byte("#cloud-config\n")}),
				pausedCluster(false),
			)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(Equal(1))
		})

		It("keeps reconciling when the Cluster has been deleted", func() {
			// A Cluster is removed while its Machines are still being torn down.
			// Failing here would block exactly the deletions that must still run.
			secretName := linkSecretName
			r := build(
				ownerMachine(&secretName),
				bootstrapSecret(secretName, map[string][]byte{bootstrapDataSecretKey: []byte("#cloud-config\n")}),
			)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(provider.Count()).To(Equal(1))
		})

		It("records the reason that is currently in effect", func() {
			// A machine paused by its own annotation and then also by its Cluster
			// must stop reporting the annotation once the annotation is removed.
			// Comparing only the condition's status would pin the first reason.
			hm.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
			r := build(ownerMachine(nil), pausedCluster(true))

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(apimeta.FindStatusCondition(reload(r).Status.Conditions,
				infrav1.MachinePausedCondition).Message).To(ContainSubstring("annotation"))

			out := reload(r)
			out.Annotations = nil
			Expect(r.Update(ctx, out)).To(Succeed())

			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(apimeta.FindStatusCondition(reload(r).Status.Conditions,
				infrav1.MachinePausedCondition).Message).To(ContainSubstring("spec.paused"))
		})
	})

	Context("owner resolution", func() {
		It("ignores owner references that are not Cluster API Machines", func() {
			hm.OwnerReferences = []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs"},
				{APIVersion: "example.com/v1", Kind: machineKind, Name: foreignMachineName},
				{APIVersion: "v1", Kind: machineKind, Name: "core-group-machine"},
			}
			Expect(ownerMachineName(hm)).To(BeEmpty())
		})

		It("matches a Machine on group alone, across contract versions", func() {
			hm.OwnerReferences = []metav1.OwnerReference{
				{APIVersion: "cluster.x-k8s.io/v1beta1", Kind: machineKind, Name: "older-contract"},
			}
			Expect(ownerMachineName(hm)).To(Equal("older-contract"))
		})
	})

	Context("watch mappings", func() {
		It("maps a Machine to the HydraMachine it references", func() {
			reqs := machineToHydraMachine(ctx, ownerMachine(nil))
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{
				Namespace: linkNamespace, Name: linkMachineName,
			}))
		})

		It("ignores Machines belonging to another infrastructure provider", func() {
			other := ownerMachine(nil)
			other.Spec.InfrastructureRef.APIGroup = "infrastructure.cluster.x-k8s.io.example"
			Expect(machineToHydraMachine(ctx, other)).To(BeEmpty())

			wrongKind := ownerMachine(nil)
			wrongKind.Spec.InfrastructureRef.Kind = "DockerMachine"
			Expect(machineToHydraMachine(ctx, wrongKind)).To(BeEmpty())
		})

		It("maps a Cluster to every HydraMachine that belongs to it", func() {
			// This is what notices a Cluster being un-paused. A paused machine does
			// not requeue, so without the mapping nothing would ever look again.
			elsewhere := linkedMachine("worker-2")
			elsewhere.Labels[clusterv1.ClusterNameLabel] = "another-cluster"

			r := build(elsewhere)
			reqs := r.clusterToHydraMachines(ctx, &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: linkClusterName, Namespace: linkNamespace},
			})

			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName.Name).To(Equal(linkMachineName))
		})
	})
})
