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
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
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

	// Provider is the infrastructure backend. Tests set it directly. In
	// production it is nil and each reconcile asks ProviderFor, for the same
	// reason the machine reconciler does it lazily -- a hypervisor that is down
	// at startup should produce a condition on an object, not a crash loop with
	// no explanation.
	Provider providers.MachineProvider

	// ProviderFor returns a connection, held for owner. See
	// HydraMachineReconciler.ProviderFor.
	ProviderFor func(ctx context.Context, owner providers.Owner, hypervisor *providers.Hypervisor) (providers.MachineProvider, error)

	// ReleaseProvider tells whatever holds connections that owner no longer
	// uses one, so a connection nobody else uses can be closed (PET-38).
	// Optional.
	ReleaseProvider func(ctx context.Context, owner providers.Owner)

	// APIReader reads hypervisor credentials Secrets, uncached, for the reason
	// HydraMachineReconciler.APIReader gives.
	APIReader client.Reader
}

// provider returns the connection to the hypervisor a HydraCluster names, or
// the manager's flag connection when it names none.
func (r *HydraClusterReconciler) provider(ctx context.Context, hydraCluster *infrav1.HydraCluster) (providers.MachineProvider, error) {
	if r.Provider != nil {
		return r.Provider, nil
	}
	if r.ProviderFor == nil {
		return nil, fmt.Errorf("%w: no infrastructure backend is configured", providers.ErrTerminal)
	}
	hypervisor, err := hypervisorOf(ctx, r.APIReader, hydraCluster)
	if err != nil {
		return nil, err
	}
	key := client.ObjectKeyFromObject(hydraCluster)
	if hypervisor == nil {
		// Named none, or no longer: a hypervisor removed before it was confirmed
		// is no longer this cluster's to hold open.
		r.release(ctx, key)
	}
	p, err := r.ProviderFor(ctx, providers.Owner{Kind: hydraClusterKind, NamespacedName: key}, hypervisor)
	if err != nil {
		return nil, fmt.Errorf("connecting to the infrastructure backend: %w", err)
	}
	return p, nil
}

// release tells the connection holder a HydraCluster no longer uses a
// connection of its own.
func (r *HydraClusterReconciler) release(ctx context.Context, hydraCluster types.NamespacedName) {
	if r.ReleaseProvider != nil {
		r.ReleaseProvider(ctx, providers.Owner{Kind: hydraClusterKind, NamespacedName: hydraCluster})
	}
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydraclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydraclusters/status,verbs=get;update;patch
// Confirming a hypervisor lists the cluster's machines and records it on them.
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachines,verbs=get;list;watch;patch
// Hypervisor credentials, read one at a time through the uncached APIReader.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile verifies a HydraCluster's infrastructure and reports it.
func (r *HydraClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	hydraCluster := &infrav1.HydraCluster{}
	if err := r.Get(ctx, req.NamespacedName, hydraCluster); err != nil {
		if apierrors.IsNotFound(err) {
			r.release(ctx, req.NamespacedName)
		}
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
		r.release(ctx, req.NamespacedName)
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

	prov, err := r.provider(ctx, hydraCluster)
	if err != nil {
		if statusErr := r.recordUnverified(ctx, hydraCluster, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		// No backend configured is terminal, and handled like a terminal
		// infrastructure failure below: the condition says an operator has to
		// act, so requeue slowly rather than retry with backoff forever.
		if errors.Is(err, providers.ErrTerminal) {
			return ctrl.Result{RequeueAfter: requeueClusterHealthy}, nil
		}
		return ctrl.Result{}, err
	}

	// A managed network's endpoint is chosen and recorded first, so that it is
	// validated below like one that was stated, and is on the spec before
	// provisioned is reported -- Cluster API copies it only after that.
	conflicted, err := r.recordDerivedEndpoint(ctx, hydraCluster, cluster)
	switch {
	case errors.Is(err, providers.ErrTerminal), errors.Is(err, errEndpointPending):
		if statusErr := r.recordUnverified(ctx, hydraCluster, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueClusterUnverified}, nil
	case err != nil:
		return ctrl.Result{}, err
	case conflicted:
		return ctrl.Result{RequeueAfter: requeueAfterConflict}, nil
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

	// A confirmation still pending is not a reason to stop machines: the one it
	// waits for is a machine being created on the flag connection, which needs
	// this cluster Ready to get there. New machines wait for confirmation on
	// their own. So it is only noted, and verification carries on.
	var pending string
	requeue := requeueClusterHealthy
	check, err := r.checkHypervisor(ctx, prov, hydraCluster, cluster)
	if errors.Is(err, errHypervisorPending) {
		pending, requeue = err.Error(), requeueClusterUnverified
	} else if err != nil {
		if statusErr := r.recordUnverified(ctx, hydraCluster, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		if errors.Is(err, providers.ErrTerminal) {
			return ctrl.Result{RequeueAfter: requeueClusterHealthy}, nil
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

	// Confirmed only now, once the hypervisor also holds the cluster's pool and
	// image. Confirming fixes spec.hypervisor, and a cluster with no machines
	// has nothing else to check it against: an address that answers but holds
	// none of this cluster's infrastructure is the likeliest typo, and this way
	// it can still be corrected.
	if check != nil {
		if err := r.commitHypervisor(ctx, hydraCluster, check); errors.Is(err, errHypervisorConflict) {
			// Someone else wrote first; a fresh read on the next pass sorts it out.
			// Not a reason to report the cluster unready and stop its machines.
			pending, requeue = err.Error(), requeueAfterConflict
		} else if err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.recordVerified(ctx, hydraCluster, pending); err != nil {
		return ctrl.Result{}, err
	}
	if requeue != requeueClusterHealthy {
		return ctrl.Result{RequeueAfter: requeue}, nil
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

// recordVerified reports that the cluster's infrastructure is usable, with a
// note when its hypervisor is not confirmed yet.
func (r *HydraClusterReconciler) recordVerified(ctx context.Context, hydraCluster *infrav1.HydraCluster, note string) error {
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
	if note != "" {
		message += "; " + note
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

// defaultAPIServerPort is the port a derived endpoint gets when none was
// stated: kubeadm's default, and the one every example here uses.
const defaultAPIServerPort = 6443

// errEndpointPending reports that a managed network's endpoint cannot be chosen
// yet, because the Cluster that might state it is not known.
var errEndpointPending = errors.New("waiting for the owning Cluster before choosing the control-plane endpoint")

// recordDerivedEndpoint fills in a managed network's control-plane endpoint when
// the spec leaves the host out, and records it on the spec (PET-42).
//
// Recorded, not merely used. Cluster API copies the endpoint out of this object
// and kubeadm is handed the same value, so an address derived on every pass but
// never written down would leave nothing holding the two together. Once it is
// on the spec the immutability rules hold a live cluster on it, whatever later
// versions of the derivation would choose.
//
// The owning Cluster is consulted first. Its own endpoint may be stated
// instead, and Cluster API then uses that one rather than copying this; choosing
// a different address here would record, permanently, an endpoint the cluster
// does not have. So that value is recorded when there is one, and nothing is
// recorded until the owner is known (errEndpointPending). Either way the value
// must pass the check every stated endpoint gets before it is written.
//
// It returns true when another write got there first; the next pass reads the
// object afresh. An error wrapping ErrTerminal is an endpoint that cannot work.
func (r *HydraClusterReconciler) recordDerivedEndpoint(ctx context.Context, hydraCluster *infrav1.HydraCluster, cluster *clusterv1.Cluster) (bool, error) {
	n := hydraCluster.Spec.ManagedNetwork
	if n == nil || hydraCluster.Spec.ControlPlaneEndpoint.Host != "" {
		return false, nil
	}
	if cluster == nil {
		return false, errEndpointPending
	}

	endpoint := cluster.Spec.ControlPlaneEndpoint
	stated := hydraCluster.Spec.ControlPlaneEndpoint.Port
	switch {
	case endpoint.IsValid() && stated != 0 && stated != endpoint.Port:
		return false, fmt.Errorf("%w: Cluster %s states controlPlaneEndpoint %s, but this HydraCluster states port %d",
			providers.ErrTerminal, cluster.Name, endpoint.String(), stated)
	case !endpoint.IsValid():
		host, err := deriveEndpointHost(n)
		if err != nil {
			return false, err
		}
		endpoint = clusterv1.APIEndpoint{Host: host, Port: stated}
		if endpoint.Port == 0 {
			endpoint.Port = defaultAPIServerPort
		}
	}
	candidate := hydraCluster.DeepCopy()
	candidate.Spec.ControlPlaneEndpoint = endpoint
	if err := validateEndpointAgainstManagedNetwork(candidate); err != nil {
		return false, err
	}

	// Optimistic, so a stale copy cannot overwrite an endpoint someone stated
	// while this was deciding.
	patch := client.MergeFromWithOptions(hydraCluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
	hydraCluster.Spec.ControlPlaneEndpoint = endpoint
	if err := r.Patch(ctx, hydraCluster, patch); err != nil {
		if apierrors.IsConflict(err) {
			return true, nil
		}
		return false, fmt.Errorf("recording the control-plane endpoint %s: %w", endpoint.String(), err)
	}
	logf.FromContext(ctx).Info("Recorded control-plane endpoint for managed network", "name", hydraCluster.Name,
		"endpoint", endpoint.String(), "statedOnCluster", cluster.Spec.ControlPlaneEndpoint.IsValid())
	return false, nil
}

// deriveEndpointHost returns the first address of a managed network that can
// carry the control-plane endpoint: not the network address, not the gateway
// libvirt puts on the first host address, not the broadcast address, and not
// inside the DHCP range. It is the inverse of
// validateEndpointAgainstManagedNetwork, which the reconciler runs on the
// result anyway.
//
// The first such address rather than any: an operator reading the spec can see
// why it was chosen, and the same network gives the same answer every time.
func deriveEndpointHost(n *infrav1.HydraManagedNetwork) (string, error) {
	gateway, subnet, err := managedNetworkGateway(n.Subnet)
	if err != nil {
		return "", err
	}
	start, end := net.ParseIP(n.DHCPStart).To4(), net.ParseIP(n.DHCPEnd).To4()
	if start == nil || end == nil {
		return "", fmt.Errorf("%w: managedNetwork DHCP bounds %q-%q are not both addresses",
			providers.ErrTerminal, n.DHCPStart, n.DHCPEnd)
	}
	candidate := nextIPv4(gateway)
	if candidate != nil && bytes.Compare(candidate, start) >= 0 && bytes.Compare(candidate, end) <= 0 {
		candidate = nextIPv4(end)
	}
	if candidate == nil || !subnet.Contains(candidate) || bytes.Compare(candidate, broadcastOf(subnet)) >= 0 {
		return "", fmt.Errorf("%w: managedNetwork.subnet %s has no address left for the control-plane endpoint: everything outside the DHCP range %s-%s is the network, gateway or broadcast address; narrow the range to leave one",
			providers.ErrTerminal, n.Subnet, n.DHCPStart, n.DHCPEnd)
	}
	return candidate.String(), nil
}

// nextIPv4 returns the address after ip, or nil after 255.255.255.255.
func nextIPv4(ip net.IP) net.IP {
	v := binary.BigEndian.Uint32(ip.To4())
	if v == math.MaxUint32 {
		return nil
	}
	out := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(out, v+1)
	return out
}

// errHypervisorPending reports that a cluster's hypervisor cannot be confirmed
// yet, for a reason that is not the cluster's to fix: a machine is still being
// created on the flag connection.
var errHypervisorPending = errors.New("hypervisor not confirmed yet")

// checkHypervisor checks, read-only, that every machine a cluster already has
// lives on the hypervisor its spec names (PET-38). commitHypervisor then
// confirms that hypervisor in status.hypervisor and records it on those
// machines -- after Reconcile has also found the cluster's infrastructure
// there. Nil when the cluster names no hypervisor.
//
// It is what makes adding spec.hypervisor to a running cluster safe. A machine
// is only ever operated on through the hypervisor it records, and one made
// before the cluster named a hypervisor records none, so it lives on the
// manager's flag connection. Recording the named hypervisor on it is the one
// moment a machine's hypervisor changes, and it happens only after that
// machine's domain -- the same name and the same ID -- was found there.
//
// Nothing is written until every machine has been checked. A machine found elsewhere,
// or recording another hypervisor, refuses confirmation terminally and nothing
// is written: the operator named the wrong hypervisor, or the machines are not
// where the provider thinks, and either needs a person. Machines not made yet
// are skipped: they place themselves on the confirmed hypervisor, and they
// wait until there is one. A machine whose Create on the flag connection has
// begun -- it records a storage pool, but has no providerID yet -- makes the
// guard wait for it rather than skip it. Machines being deleted are skipped
// too: their teardown goes wherever they record, and moving it mid-way would
// not help.
//
// Confirming comes before recording, so a spec edit can never land between the
// two and leave machines recorded against a hypervisor the cluster no longer
// names: confirmation fixes the spec (see the CEL rule on HydraCluster), and it
// is written with an optimistic lock, so it cannot confirm a spec that changed
// since it was read. It runs on every reconcile, which is how a machine that
// gained its providerID after confirmation is checked and recorded too.
func (r *HydraClusterReconciler) checkHypervisor(
	ctx context.Context, prov providers.MachineProvider, hydraCluster *infrav1.HydraCluster, cluster *clusterv1.Cluster,
) (*hypervisorCheck, error) {
	if hydraCluster.Spec.Hypervisor == nil {
		return nil, nil
	}
	address, err := canonicalHypervisor(hydraCluster.Spec.Hypervisor.Address)
	if err != nil {
		return nil, err
	}
	if cluster == nil {
		// Its machines are found through the Cluster's name, and there is no
		// telling which machines are its own until Cluster API sets the owner.
		return nil, errors.New("waiting for the owning Cluster before confirming the hypervisor")
	}

	machines := &infrav1.HydraMachineList{}
	if err := r.List(ctx, machines, client.InNamespace(hydraCluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name}); err != nil {
		return nil, fmt.Errorf("listing the cluster's machines: %w", err)
	}
	var unrecorded []*infrav1.HydraMachine
	var refused, pending []string
	for i := range machines.Items {
		machine := &machines.Items[i]
		if !machine.DeletionTimestamp.IsZero() {
			continue
		}
		switch recorded := machine.Annotations[infrav1.HypervisorAnnotation]; {
		case recorded == address:
			continue
		case recorded != "":
			refused = append(refused, fmt.Sprintf("%s records hypervisor %s", machine.Name, recorded))
			continue
		}
		if machine.Spec.ProviderID == nil || *machine.Spec.ProviderID == "" {
			if mayBeMadeOnFlags(machine) {
				// A Create on the flag connection has begun and not finished, or
				// finished without its providerID being recorded. Confirming now
				// would let it come back as a machine elsewhere with no record, so
				// wait for it to be made, and checked like any other. Collected,
				// not returned: a refusal elsewhere in the cluster still has to be
				// found, and it outranks this.
				pending = append(pending, machine.Name)
			}
			continue
		}
		refusal, err := machineOnHypervisor(ctx, prov, machine)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			refused = append(refused, refusal)
			continue
		}
		unrecorded = append(unrecorded, machine)
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("%w: hypervisor %s cannot be confirmed for this cluster, because not all its machines are there (%s); "+
			"machines never move between hypervisors, so name the one they are on",
			providers.ErrTerminal, address, summarize(refused, "; "))
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("%w: waiting for HydraMachine %s, being created on the manager's flag connection",
			errHypervisorPending, summarize(pending, ", "))
	}

	return &hypervisorCheck{address: address, unrecorded: unrecorded}, nil
}

// hypervisorCheck is what checkHypervisor found: the canonical address, and
// the machines to record it on once it is confirmed.
type hypervisorCheck struct {
	address    string
	unrecorded []*infrav1.HydraMachine
}

// errHypervisorConflict reports that confirming lost an optimistic-lock race
// with another writer. Benign: the next pass reads afresh.
var errHypervisorConflict = errors.New("hypervisor confirmation conflicted with another update; retrying")

// requeueAfterConflict is how soon to retry after errHypervisorConflict.
const requeueAfterConflict = 2 * time.Second

// commitHypervisor confirms the hypervisor checkHypervisor found -- writes
// status.hypervisor, which fixes the spec -- and then records it on the
// machines that lacked it. See checkHypervisor for why in that order.
//
// A conflict on either write is errHypervisorConflict, after the other
// machines have been tried: one machine whose status the machine reconciler
// rewrote meanwhile must not hold up the rest.
func (r *HydraClusterReconciler) commitHypervisor(ctx context.Context, hydraCluster *infrav1.HydraCluster, check *hypervisorCheck) error {
	address := check.address
	if !confirmedHypervisor(hydraCluster) {
		previous := hydraCluster.Status.Hypervisor
		patch := client.MergeFromWithOptions(hydraCluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
		hydraCluster.Status.Hypervisor = &infrav1.HydraHypervisorStatus{Address: hydraCluster.Spec.Hypervisor.Address}
		if err := r.Status().Patch(ctx, hydraCluster, patch); err != nil {
			// Not confirmed, so later status writes in this reconcile must not
			// claim it either.
			hydraCluster.Status.Hypervisor = previous
			if apierrors.IsConflict(err) {
				return fmt.Errorf("%w: confirming hypervisor %s", errHypervisorConflict, address)
			}
			return fmt.Errorf("confirming hypervisor %s: %w", address, err)
		}
		logf.FromContext(ctx).Info("Confirmed hypervisor", "hypervisor", address)
	}
	var conflicted []string
	for _, machine := range check.unrecorded {
		// Optimistic, so a stale copy cannot overwrite an annotation the machine
		// reconciler has just written.
		patch := client.MergeFromWithOptions(machine.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if machine.Annotations == nil {
			machine.Annotations = map[string]string{}
		}
		machine.Annotations[infrav1.HypervisorAnnotation] = address
		if err := r.Patch(ctx, machine, patch); err != nil {
			if apierrors.IsConflict(err) {
				conflicted = append(conflicted, machine.Name)
				continue
			}
			return fmt.Errorf("recording hypervisor %s on HydraMachine %s: %w", address, machine.Name, err)
		}
		logf.FromContext(ctx).Info("Recorded hypervisor on machine", "hypervisor", address, "hydraMachine", machine.Name)
	}
	if len(conflicted) > 0 {
		return fmt.Errorf("%w: recording hypervisor %s on %s", errHypervisorConflict, address, summarize(conflicted, ", "))
	}
	return nil
}

// machineOnHypervisor checks that a machine's domain is on the hypervisor prov
// reaches: found by name, and with the ID its providerID records, so a domain
// that merely shares the name is not mistaken for it. It returns why not, or
// an error when the hypervisor could not be asked.
func machineOnHypervisor(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine) (string, error) {
	backend, id, err := providers.ParseProviderID(*machine.Spec.ProviderID)
	if err != nil || backend != prov.Name() {
		return fmt.Sprintf("%s has providerID %q, which this backend cannot look up", machine.Name, *machine.Spec.ProviderID), nil
	}
	state, err := prov.FindByName(ctx, backendName(machine))
	switch {
	case errors.Is(err, providers.ErrNotFound):
		return fmt.Sprintf("%s has no domain named %s there", machine.Name, backendName(machine)), nil
	case err != nil:
		return "", fmt.Errorf("looking for HydraMachine %s on the hypervisor: %w", machine.Name, err)
	case state.ID != id:
		return fmt.Sprintf("%s's domain %s there has ID %s, not the recorded %s", machine.Name, backendName(machine), state.ID, id), nil
	}
	return "", nil
}

// maxListed is how many machines a condition message names before it only
// counts the rest.
const maxListed = 5

// summarize joins items, naming at most maxListed and counting the rest, so a
// message about a large cluster stays far inside a condition message's
// 32768-character limit -- past which the status write itself is refused, and
// the condition that should say what is wrong is never recorded.
func summarize(items []string, sep string) string {
	if len(items) <= maxListed {
		return strings.Join(items, sep)
	}
	return fmt.Sprintf("%s%sand %d more, %d in all", strings.Join(items[:maxListed], sep), sep, len(items)-maxListed, len(items))
}
