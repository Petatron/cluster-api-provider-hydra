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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// HydraMachineTemplateReconciler reconciles a HydraMachineTemplate object.
//
// It publishes one thing: the capacity and platform of the node a machine
// cloned from this template would become. That is the whole InfraMachineTemplate
// contract, and it exists for exactly one consumer -- Cluster Autoscaler sizing
// a node pool that currently has no replicas. With zero replicas there is no
// Node to inspect, so the numbers have to come from the template or the pool
// cannot be scaled up at all.
//
// Notably absent: any call to the infrastructure backend. Capacity is a pure
// function of an immutable spec, so there is nothing to ask a hypervisor and
// nothing that can change once answered. Dialling would only add a way for a
// transient libvirt outage to stop an autoscaler from sizing a pool.
type HydraMachineTemplateReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Platform is the node platform the configured backend produces. Injected
	// rather than looked up so this controller keeps no backend dependency, and
	// so a test can assert a non-amd64 pool reports itself as one.
	Platform providers.NodePlatform
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachinetemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachinetemplates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch

// Reconcile publishes a HydraMachineTemplate's capacity and node platform.
func (r *HydraMachineTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	template := &infrav1.HydraMachineTemplate{}
	if err := r.Get(ctx, req.NamespacedName, template); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// No finalizer: this controller creates nothing outside the object's own
	// status, and unlike the cluster reconciler that is a tautology rather than a
	// policy -- there is no backend call here that could leave anything behind.
	if !template.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Pausing is honoured here exactly as it is on the machine and cluster
	// reconcilers, and the earlier argument for exempting this one was wrong on
	// both halves: skipping a reconcile does not withdraw capacity -- status keeps
	// whatever it already published -- and pause is not only for protecting
	// infrastructure. clusterctl move pauses a Cluster precisely so that no
	// controller writes to objects mid-migration, and a status write is a write.
	cluster, err := ownerClusterOf(ctx, r.Client, template)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason := pausedReason(template.Annotations, cluster); reason != "" {
		log.V(1).Info("Reconciliation is paused", "name", template.Name, "reason", reason)
		if err := r.setPaused(ctx, template, reason); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err := r.setPaused(ctx, template, ""); err != nil {
		return ctrl.Result{}, err
	}

	capacity := capacityFor(template)
	nodeInfo := infrav1.HydraNodeInfo{
		Architecture:    infrav1.HydraNodeArchitecture(r.Platform.Architecture),
		OperatingSystem: r.Platform.OperatingSystem,
	}

	// Only write when something would change. This controller watches its own
	// kind, and a status patch produces an event for the object that was patched,
	// so an unconditional write is an unconditional loop.
	if capacityEqual(template.Status.Capacity, capacity) && template.Status.NodeInfo == nodeInfo {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(template.DeepCopy())
	template.Status.Capacity = capacity
	template.Status.NodeInfo = nodeInfo
	if err := r.Status().Patch(ctx, template, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("publishing template capacity: %w", err)
	}

	cpu, memory := capacity[corev1.ResourceCPU], capacity[corev1.ResourceMemory]
	log.V(1).Info("Published HydraMachineTemplate capacity",
		"name", template.Name,
		"cpu", cpu.String(),
		"memory", memory.String(),
		"architecture", nodeInfo.Architecture,
	)
	return ctrl.Result{}, nil
}

// capacityFor derives the capacity of the node a machine from this template
// becomes.
//
// These are raw machine sizes, and they are an UPPER BOUND on the node rather
// than a prediction of it. Cluster Autoscaler simulates a node whose allocatable
// equals capacity, and measured on hardware a 4Gi/40Gi machine yields a node
// with 6.8% less allocatable memory and 15.2% less allocatable ephemeral storage
// than published here. Two losses stack: the guest kernel does not see all the
// RAM it was given and the root filesystem is smaller than the raw disk (~4-6%,
// before the kubelet is involved), then the eviction thresholds take 100Mi and
// 10% on top.
//
// Raw is still what gets published, because the first loss is a property of the
// guest image and its kernel and this provider cannot know it. A hardcoded
// reduction would be correct for one image at one disk size and wrong for the
// next -- a guess wearing the costume of a measurement. The capacity annotations
// are the per-pool correction, where the image IS known.
// docs/scale-from-zero.md carries the measurements.
//
// maxPods is not reported. The autoscaler overwrites the pods entry
// unconditionally, from its own annotation or its default of 110, so a value
// here would be read and discarded -- precise-looking and inert.
//
// Extended resources, GPUs included, are not reported either: nothing in the
// machine spec declares any, and PET-33 has not yet decided how libvirt exposes
// a GPU. The capacity annotations cover a min=0 GPU pool in the meantime, and
// the autoscaler accepts arbitrary resource names from this map, so adding them
// later breaks nothing.
func capacityFor(template *infrav1.HydraMachineTemplate) infrav1.HydraNodeCapacity {
	spec := template.Spec.Template.Spec
	return infrav1.HydraNodeCapacity{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(spec.VCPUs), resource.DecimalSI),
		corev1.ResourceMemory: spec.Memory.DeepCopy(),

		// The root disk is where ephemeral storage comes from, so this is the
		// right number to report, but it is an upper bound: the node's
		// ephemeral-storage capacity is the filesystem holding /var/lib/kubelet,
		// which is smaller than the raw disk by the partition table, /boot and
		// filesystem overhead.
		corev1.ResourceEphemeralStorage: spec.DiskSize.DeepCopy(),
	}
}

// capacityEqual compares two capacity maps by quantity value.
//
// Not reflect.DeepEqual: resource.Quantity carries a cached string form and a
// format, so "8Gi" and "8589934592" are different structs holding the same
// quantity. Comparing structs would rewrite status forever on a template whose
// spec was applied in a different but equivalent notation.
func capacityEqual(a, b infrav1.HydraNodeCapacity) bool {
	if len(a) != len(b) {
		return false
	}
	for name, want := range b {
		got, ok := a[name]
		if !ok || got.Cmp(want) != 0 {
			return false
		}
	}
	return true
}

// setPaused surfaces whether reconciliation is suspended, and why.
//
// Mirrors the cluster and machine reconcilers rather than leaving an empty
// status to be interpreted. A paused template that has never published capacity
// is otherwise indistinguishable from a pool the autoscaler simply will not
// scale, since the autoscaler skips an unsized node group without logging at
// default verbosity.
func (r *HydraMachineTemplateReconciler) setPaused(ctx context.Context, template *infrav1.HydraMachineTemplate, reason string) error {
	return setPausedCondition(ctx, r.Client, template, &template.Status.Conditions,
		infrav1.MachineTemplatePausedCondition, reason)
}

// clusterToHydraMachineTemplates enqueues templates owned by a changed Cluster,
// so both pause transitions are observed even when a template is otherwise idle.
func (r *HydraMachineTemplateReconciler) clusterToHydraMachineTemplates(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}
	templates := &infrav1.HydraMachineTemplateList{}
	if err := r.List(ctx, templates, client.InNamespace(cluster.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list HydraMachineTemplates for a Cluster event", "cluster", cluster.Name)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(templates.Items))
	for i := range templates.Items {
		template := &templates.Items[i]
		// Match ownerClusterOf, including its protection against name reuse.
		// Templates need not carry the cluster-name label or a controller owner.
		ref := ownerRefOfKind(template.OwnerReferences, clusterKind)
		if ref == nil || ref.Name != cluster.Name || (ref.UID != "" && ref.UID != cluster.UID) {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(template)})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *HydraMachineTemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HydraMachineTemplate{}).
		Named("hydramachinetemplate").
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToHydraMachineTemplates),
		).
		Complete(r)
}
