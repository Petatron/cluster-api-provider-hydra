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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

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

	// No finalizer, and this is now a policy rather than a tautology.
	//
	// It used to be simply true that there was nothing to release: the storage
	// pool and base image were there before the controller looked and outlive it.
	// A managed network changes that -- Hydra may have defined it -- so deletion
	// now LEAKS a libvirt network, on purpose, and that is worth stating rather
	// than discovering.
	//
	// Tearing one down correctly needs two things this controller does not have.
	// It needs to know whether Hydra created the network or merely adopted one an
	// operator built, because deleting the latter would take down every guest
	// already attached to it, including guests belonging to nobody here. And it
	// needs to wait until no machine still holds an interface on it, which means
	// ordering cluster teardown behind machine teardown. Getting that order wrong
	// destroys running clusters; a leftover virbr costs an operator one
	// `virsh net-undefine`.
	//
	// So the leak is the deliberate choice until ownership is tracked. See PET-41.
	if !hydraCluster.DeletionTimestamp.IsZero() {
		log.V(1).Info("HydraCluster is being deleted; any managed network is deliberately left in place",
			"name", hydraCluster.Name)
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

	// Before the backend is asked for anything. This is a property of the
	// declaration alone, so a cluster that cannot work should say so without
	// first creating a network for it.
	if err := validateEndpointAgainstManagedNetwork(hydraCluster); err != nil {
		if statusErr := r.recordUnverified(ctx, hydraCluster, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueClusterUnverified}, nil
	}

	if err := prov.EnsureInfrastructure(ctx, infrastructureSpecFor(hydraCluster)); err != nil {
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
	return ownerClusterOf(ctx, r.Client, hydraCluster)
}

// infrastructureSpecFor converts the API object into what the backend checks.
func infrastructureSpecFor(hydraCluster *infrav1.HydraCluster) providers.InfrastructureSpec {
	spec := providers.InfrastructureSpec{StoragePool: hydraCluster.Spec.StoragePool}
	if img := hydraCluster.Spec.BaseImage; img != nil {
		spec.Image = providers.Image{Name: img.Name, URL: img.URL, Checksum: img.Checksum}
	}
	spec.ManagedNetwork = managedNetworkOf(hydraCluster)
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
	return setPausedCondition(ctx, r.Client, hydraCluster, &hydraCluster.Status.Conditions,
		infrav1.ClusterPausedCondition, reason)
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

// managedNetworkOf converts the API's managed-network declaration for the
// backend, or nil when the cluster declares none.
//
// Shared by the cluster check and machine creation on purpose: the network the
// cluster verifies and the network machines are attached to must be the same
// one, and two conversions could drift.
func managedNetworkOf(hydraCluster *infrav1.HydraCluster) *providers.ManagedNetwork {
	if hydraCluster == nil || hydraCluster.Spec.ManagedNetwork == nil {
		return nil
	}
	n := hydraCluster.Spec.ManagedNetwork
	return &providers.ManagedNetwork{
		Name:      n.Name,
		Subnet:    n.Subnet,
		DHCPStart: n.DHCPStart,
		DHCPEnd:   n.DHCPEnd,
	}
}

// validateEndpointAgainstManagedNetwork checks the one invariant a managed
// network exists to provide: that the control-plane endpoint is an address the
// network can actually carry and will never hand to a machine.
//
// Nothing else enforces it. The subnet, the range and the endpoint are three
// independently valid fields, and CEL cannot compare them -- ordering IP
// addresses is not something the expression language can express -- so a cluster
// declaring an endpoint inside its own DHCP range would admit cleanly and then
// hand that address to the first machine that asked.
//
// Which is the whole feature failing quietly, so it is checked loudly instead.
// Terminal: every one of these is a statement about the spec, and
// controlPlaneEndpoint is immutable, so none of them improves by waiting.
func validateEndpointAgainstManagedNetwork(hydraCluster *infrav1.HydraCluster) error {
	n := hydraCluster.Spec.ManagedNetwork
	if n == nil {
		return nil
	}
	host := hydraCluster.Spec.ControlPlaneEndpoint.Host
	endpoint := net.ParseIP(host)
	if endpoint == nil {
		// A name rather than an address is not something this can check, and not
		// something to refuse: an operator may be fronting the endpoint with DNS
		// they manage. The range guarantee is theirs to keep in that case.
		return nil
	}

	_, subnet, err := net.ParseCIDR(n.Subnet)
	if err != nil {
		return fmt.Errorf("%w: managedNetwork.subnet %q is not valid CIDR: %v",
			providers.ErrTerminal, n.Subnet, err)
	}
	if !subnet.Contains(endpoint) {
		return fmt.Errorf("%w: controlPlaneEndpoint.host %s is not inside managedNetwork.subnet %s, so nothing on that network can answer for it",
			providers.ErrTerminal, host, n.Subnet)
	}

	// The addresses outside the DHCP range are not all free. libvirt takes the
	// first host address for the gateway, and the network and broadcast
	// addresses were never usable. An endpoint on any of them is unreachable in
	// a way that looks like the VIP simply not working.
	gateway, _, err := managedNetworkGateway(n.Subnet)
	if err != nil {
		return err
	}
	switch {
	case endpoint.Equal(subnet.IP):
		return fmt.Errorf("%w: controlPlaneEndpoint.host %s is the network address of managedNetwork.subnet %s",
			providers.ErrTerminal, host, n.Subnet)
	case endpoint.Equal(gateway):
		return fmt.Errorf("%w: controlPlaneEndpoint.host %s is the gateway of managedNetwork.subnet %s; libvirt takes the first host address",
			providers.ErrTerminal, host, n.Subnet)
	case endpoint.Equal(broadcastOf(subnet)):
		return fmt.Errorf("%w: controlPlaneEndpoint.host %s is the broadcast address of managedNetwork.subnet %s",
			providers.ErrTerminal, host, n.Subnet)
	}

	start, end := net.ParseIP(n.DHCPStart), net.ParseIP(n.DHCPEnd)
	if start == nil || end == nil {
		return fmt.Errorf("%w: managedNetwork DHCP bounds %q-%q are not both addresses",
			providers.ErrTerminal, n.DHCPStart, n.DHCPEnd)
	}
	if bytes.Compare(endpoint.To4(), start.To4()) >= 0 && bytes.Compare(endpoint.To4(), end.To4()) <= 0 {
		return fmt.Errorf("%w: controlPlaneEndpoint.host %s is inside managedNetwork's DHCP range %s-%s, so a machine can be given the endpoint's address; choose an address in %s outside that range",
			providers.ErrTerminal, host, n.DHCPStart, n.DHCPEnd, n.Subnet)
	}
	return nil
}

// managedNetworkGateway mirrors the backend's choice of gateway so the two
// cannot disagree about which address is reserved.
func managedNetworkGateway(cidr string) (net.IP, *net.IPNet, error) {
	_, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: managedNetwork.subnet %q is not valid CIDR: %v",
			providers.ErrTerminal, cidr, err)
	}
	base := subnet.IP.To4()
	if base == nil {
		return nil, nil, fmt.Errorf("%w: managedNetwork.subnet %q is not IPv4", providers.ErrTerminal, cidr)
	}
	gw := make(net.IP, len(base))
	copy(gw, base)
	gw[3]++
	return gw, subnet, nil
}

func broadcastOf(subnet *net.IPNet) net.IP {
	ip := subnet.IP.To4()
	mask := subnet.Mask
	if ip == nil || len(mask) != net.IPv4len {
		return nil
	}
	out := make(net.IP, net.IPv4len)
	for i := range out {
		out[i] = ip[i] | ^mask[i]
	}
	return out
}
