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
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// clusterKind is the Cluster API kind that owns a HydraCluster.
const clusterKind = "Cluster"

// requeueClusterHealthy is how often verified cluster infrastructure is
// re-checked.
//
// Verification is a point-in-time statement, and the things it checks can go
// away underneath us -- a pool can be stopped, an image deleted. Re-checking
// turns "this was true once" into something an operator can rely on, and it is
// two RPCs, so it can afford to be frequent-ish without being noisy.
const requeueClusterHealthy = 5 * time.Minute

// requeueClusterUnverified is how soon to retry infrastructure that could not be
// verified for a reason that may pass, such as an unreachable hypervisor.
const requeueClusterUnverified = 30 * time.Second

// HydraClusterReconciler reconciles a HydraCluster object.
//
// Its job is smaller than the machine reconciler's and worth stating plainly:
// Hydra creates no cluster-scoped infrastructure. There is no network to build,
// no load balancer to stand up. What this controller does is *verify* that the
// infrastructure the cluster was pointed at actually exists, and report that
// through the contract so machine provisioning is gated on it.
type HydraClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Provider is the infrastructure backend. Tests set it directly; in
	// production it is built on first use from NewProvider, for the same reason
	// the machine reconciler does it lazily -- a hypervisor that is down at
	// startup should produce a condition on an object, not a crash loop with no
	// explanation.
	Provider providers.MachineProvider

	// NewProvider builds the backend on first reconcile.
	NewProvider func(context.Context) (providers.MachineProvider, error)

	mu sync.Mutex
}

func (r *HydraClusterReconciler) provider(ctx context.Context) (providers.MachineProvider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.Provider != nil {
		return r.Provider, nil
	}
	if r.NewProvider == nil {
		return nil, fmt.Errorf("%w: no infrastructure backend is configured", providers.ErrTerminal)
	}
	p, err := r.NewProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to the infrastructure backend: %w", err)
	}
	r.Provider = p
	return p, nil
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydraclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydraclusters/status,verbs=get;update;patch

// Reconcile verifies a HydraCluster's infrastructure and reports it.
func (r *HydraClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	hydraCluster := &infrav1.HydraCluster{}
	if err := r.Get(ctx, req.NamespacedName, hydraCluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// No finalizer, deliberately.
	//
	// A finalizer exists to hold an object open while its provider tears
	// something down. This provider creates nothing at cluster scope -- the
	// storage pool and the base image were there before it looked and outlive it
	// -- so a finalizer here would guard nothing while adding a way for deletion
	// to wedge. It earns its place when there is something to release; see
	// PET-38, which introduces per-cluster hypervisor connections.
	if !hydraCluster.DeletionTimestamp.IsZero() {
		log.V(1).Info("HydraCluster is being deleted; nothing to release", "name", hydraCluster.Name)
		return ctrl.Result{}, nil
	}

	cluster, err := r.ownerCluster(ctx, hydraCluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	if reason := pausedReason(hydraCluster.Annotations, cluster); reason != "" {
		log.V(1).Info("Reconciliation is paused", "name", hydraCluster.Name, "reason", reason)
		return ctrl.Result{}, r.setPaused(ctx, hydraCluster, reason)
	}
	if err := r.setPaused(ctx, hydraCluster, ""); err != nil {
		return ctrl.Result{}, err
	}

	prov, err := r.provider(ctx)
	if err != nil {
		if statusErr := r.recordUnverified(ctx, hydraCluster, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if err := prov.CheckInfrastructure(ctx, infrastructureSpecFor(hydraCluster)); err != nil {
		if statusErr := r.recordUnverified(ctx, hydraCluster, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		// A terminal failure is not worth returning as an error: the controller
		// would retry it with backoff forever, and the condition already says an
		// operator has to act. Requeue slowly instead so the object recovers on
		// its own once they do.
		if errors.Is(err, providers.ErrTerminal) {
			return ctrl.Result{RequeueAfter: requeueClusterHealthy}, nil
		}
		return ctrl.Result{RequeueAfter: requeueClusterUnverified}, nil
	}

	if err := r.recordVerified(ctx, hydraCluster); err != nil {
		return ctrl.Result{}, err
	}
	// Re-check periodically. Verification is point-in-time and the things it
	// checks can be removed while the cluster is running.
	return ctrl.Result{RequeueAfter: requeueClusterHealthy}, nil
}

// ownerCluster returns the Cluster that owns this object, or nil.
//
// Absent is normal rather than exceptional: Cluster API sets the owner reference
// shortly after the object appears, and verification does not depend on the
// Cluster anyway. The only thing a missing owner costs is the paused check.
func (r *HydraClusterReconciler) ownerCluster(ctx context.Context, hydraCluster *infrav1.HydraCluster) (*clusterv1.Cluster, error) {
	ref := ownerRefOfKind(hydraCluster.OwnerReferences, clusterKind)
	if ref == nil {
		return nil, nil
	}

	cluster := &clusterv1.Cluster{}
	key := types.NamespacedName{Namespace: hydraCluster.Namespace, Name: ref.Name}
	if err := r.Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			logf.FromContext(ctx).V(1).Info("Owner Cluster is not visible", "cluster", ref.Name)
			return nil, nil
		}
		return nil, fmt.Errorf("reading owner Cluster %q: %w", ref.Name, err)
	}
	// A name is not an identity -- same reasoning as the machine reconciler's
	// owner check. A Cluster deleted and recreated under the same name is a
	// different object, and pausing decisions must not follow it.
	if ref.UID != "" && cluster.UID != ref.UID {
		logf.FromContext(ctx).V(1).Info("Owner Cluster name is reused by a different object", "cluster", ref.Name)
		return nil, nil
	}
	return cluster, nil
}

// infrastructureSpecFor converts the API object into what the backend checks.
func infrastructureSpecFor(hydraCluster *infrav1.HydraCluster) providers.InfrastructureSpec {
	spec := providers.InfrastructureSpec{StoragePool: hydraCluster.Spec.StoragePool}
	if img := hydraCluster.Spec.BaseImage; img != nil {
		spec.Image = providers.Image{Name: img.Name, URL: img.URL, Checksum: img.Checksum}
	}
	return spec
}

// recordVerified reports that the cluster's infrastructure is usable.
func (r *HydraClusterReconciler) recordVerified(ctx context.Context, hydraCluster *infrav1.HydraCluster) error {
	patch := client.MergeFrom(hydraCluster.DeepCopy())

	provisioned := true
	hydraCluster.Status.Initialization.Provisioned = &provisioned

	// Say what was actually checked. The image prerequisite is skipped when the
	// cluster named no default -- claiming "base image present" in that case
	// would be a plain untruth on the object an operator reads first.
	message := "storage pool is running"
	if hydraCluster.Spec.BaseImage != nil {
		message = "storage pool is running and the base image is present in it"
	}

	apimeta.SetStatusCondition(&hydraCluster.Status.Conditions, metav1.Condition{
		Type:               infrav1.ClusterReadyCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "InfrastructureVerified",
		Message:            message,
		ObservedGeneration: hydraCluster.Generation,
	})
	apimeta.RemoveStatusCondition(&hydraCluster.Status.Conditions, infrav1.ClusterInfrastructureFailedCondition)

	if err := r.Status().Patch(ctx, hydraCluster, patch); err != nil {
		return fmt.Errorf("recording verified infrastructure: %w", err)
	}
	return nil
}

// recordUnverified reports that the cluster's infrastructure could not be
// confirmed, distinguishing what an operator must fix from what may pass.
//
// initialization.provisioned is deliberately never set back to false once true.
// It is an initialization milestone that Cluster API orchestrates on, and
// regressing it would tell CAPI the cluster is being provisioned again. Ongoing
// health is what the Ready condition is for -- the same split the machine
// reconciler already draws.
func (r *HydraClusterReconciler) recordUnverified(ctx context.Context, hydraCluster *infrav1.HydraCluster, cause error) error {
	terminal := errors.Is(cause, providers.ErrTerminal)

	reason := "InfrastructureUnverified"
	if terminal {
		reason = "InfrastructureInvalid"
	}

	patch := client.MergeFrom(hydraCluster.DeepCopy())

	if hydraCluster.Status.Initialization.Provisioned == nil {
		provisioned := false
		hydraCluster.Status.Initialization.Provisioned = &provisioned
	}

	apimeta.SetStatusCondition(&hydraCluster.Status.Conditions, metav1.Condition{
		Type:               infrav1.ClusterReadyCondition,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: hydraCluster.Generation,
	})
	if terminal {
		apimeta.SetStatusCondition(&hydraCluster.Status.Conditions, metav1.Condition{
			Type:               infrav1.ClusterInfrastructureFailedCondition,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            cause.Error(),
			ObservedGeneration: hydraCluster.Generation,
		})
	} else {
		apimeta.RemoveStatusCondition(&hydraCluster.Status.Conditions, infrav1.ClusterInfrastructureFailedCondition)
	}

	if err := r.Status().Patch(ctx, hydraCluster, patch); err != nil {
		return fmt.Errorf("recording unverified infrastructure: %w", err)
	}
	return nil
}

// setPaused surfaces whether reconciliation is suspended, and why.
func (r *HydraClusterReconciler) setPaused(ctx context.Context, hydraCluster *infrav1.HydraCluster, reason string) error {
	paused := reason != ""

	existing := apimeta.FindStatusCondition(hydraCluster.Status.Conditions, infrav1.ClusterPausedCondition)
	if !paused && existing == nil {
		return nil
	}

	cond := metav1.Condition{
		Type:               infrav1.ClusterPausedCondition,
		Status:             metav1.ConditionFalse,
		Reason:             "NotPaused",
		Message:            "reconciliation is active",
		ObservedGeneration: hydraCluster.Generation,
	}
	if paused {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Paused"
		cond.Message = fmt.Sprintf("reconciliation is suspended by %s", reason)
	}

	// Compare the message too, not just the status, so a machine paused by two
	// signals reports the one currently in effect.
	if existing != nil && existing.Status == cond.Status && existing.Message == cond.Message {
		return nil
	}

	patch := client.MergeFrom(hydraCluster.DeepCopy())
	apimeta.SetStatusCondition(&hydraCluster.Status.Conditions, cond)
	if err := r.Status().Patch(ctx, hydraCluster, patch); err != nil {
		return fmt.Errorf("recording paused state: %w", err)
	}
	return nil
}

// clusterToHydraCluster maps a Cluster to the HydraCluster it references.
//
// Without it, un-pausing a Cluster would leave its HydraCluster paused until the
// next periodic re-check, and the machines gated on it waiting alongside.
func clusterToHydraCluster(_ context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}
	ref := cluster.Spec.InfrastructureRef
	if ref.Kind != hydraClusterKind || ref.APIGroup != infrav1.GroupVersion.Group || ref.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      ref.Name,
	}}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *HydraClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HydraCluster{}).
		Named("hydracluster").
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(clusterToHydraCluster),
		).
		Complete(r)
}
