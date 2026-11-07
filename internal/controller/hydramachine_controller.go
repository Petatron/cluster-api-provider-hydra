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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/metrics"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// MachineFinalizer keeps a HydraMachine around until its backing infrastructure
// has been removed. Without it, deleting the Kubernetes object would orphan a
// running VM that nothing references and nothing will ever clean up.
const MachineFinalizer = "hydramachine.infrastructure.cluster.x-k8s.io/finalizer"

// maxReadableNamePrefix bounds the human-readable half of a backend machine
// name. With the 20-character digest and separators this keeps the whole name
// under 64 characters, comfortably inside the 255-byte filename limit that
// directory-backed libvirt pools impose once ".qcow2" is appended.
const maxReadableNamePrefix = 40

// maxHostnameLength is the longest hostname a Linux guest will accept. The
// kernel's HOST_NAME_MAX is 64 including the terminator, and a DNS label is
// capped at the same 63 characters, so one bound covers both.
const maxHostnameLength = 63

// maxStatusAddresses mirrors the MaxItems on status.addresses in the CRD. The
// two must stay in step: exceeding it makes the API server reject the patch.
const maxStatusAddresses = 32

// requeueWhileProvisioning is how long to wait before re-checking a machine that
// exists but is not yet reporting ready.
const requeueWhileProvisioning = 15 * time.Second

// requeueWhileHealthy is how often to re-check a machine that already looks
// fine. Once Ready and addressed, there is no backend event source, so without
// this a vanished or stopped VM would stay Ready=True until an unrelated
// Kubernetes event or controller-runtime's roughly 10-hour cache resync.
const requeueWhileHealthy = 5 * time.Minute

// HydraMachineReconciler reconciles a HydraMachine object
type HydraMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads directly from the API server, bypassing the shared cache.
	//
	// It exists for exactly one kind: bootstrap data Secrets. A cached read would
	// start a cluster-wide Secret informer on first use and retain every Secret
	// in the cluster in this process's memory -- for the sake of one key from a
	// handful of them. See bootstrapDataFor.
	APIReader client.Reader

	// Provider is the infrastructure backend. It is an interface so the
	// reconciler can be tested exhaustively against a fake, and so a second
	// backend requires no controller changes.
	//
	// Tests set this directly, and it then serves every machine. In production
	// it is left nil and each reconcile asks ProviderFor.
	Provider providers.MachineProvider

	// ProviderFor returns a connection to hypervisor, held for owner, or the
	// manager's flag-configured connection when hypervisor is nil. Connections
	// are held by whatever implements it, not here: a manager can reach more than
	// one hypervisor (PET-38), so there is no single provider for this reconciler
	// to keep. Each machine is an owner of the connection it records, so a
	// connection outlives its cluster's release for as long as a machine still
	// needs it for teardown.
	//
	// Connecting lazily rather than at startup is deliberate. Building it in
	// main() meant a missing storage pool, or a hypervisor that was simply down,
	// crash-looped the manager before its health probes ever came up -- so the
	// operator saw a restart count instead of a message, and the checked-in e2e
	// suite could never observe a Running manager at all. Failing here instead
	// puts the reason on the object as a terminal condition, where it is visible
	// and survives being read later.
	ProviderFor func(ctx context.Context, owner providers.Owner, hypervisor *providers.Hypervisor) (providers.MachineProvider, error)

	// ReleaseProvider tells whatever holds connections that owner no longer
	// uses one: a machine that is gone. Optional.
	ReleaseProvider func(ctx context.Context, owner providers.Owner)
}

// provider returns the connection to the hypervisor a machine lives on: the one
// placement names, or the manager's flag connection when it names none.
//
// A failure is not remembered: the next reconcile asks again, so a hypervisor
// that was unreachable is retried rather than remembered as broken.
func (r *HydraMachineReconciler) provider(ctx context.Context, machine *infrav1.HydraMachine, link *linkage, placement string) (providers.MachineProvider, error) {
	if r.Provider != nil {
		return r.Provider, nil
	}
	if r.ProviderFor == nil {
		return nil, fmt.Errorf("%w: no infrastructure backend is configured", providers.ErrTerminal)
	}
	hypervisor, err := r.hypervisorFor(ctx, link, placement)
	if err != nil {
		return nil, err
	}
	owner := machineOwner(client.ObjectKeyFromObject(machine))
	if hypervisor == nil {
		// The flag connection has no owners. Release in case this machine held
		// another, which only a hand-edited annotation can cause.
		r.release(ctx, owner)
	}
	p, err := r.ProviderFor(ctx, owner, hypervisor)
	if err != nil {
		return nil, fmt.Errorf("connecting to the infrastructure backend: %w", err)
	}
	return p, nil
}

// hypervisorFor resolves placement into a connection request, with credentials
// from the machine's HydraCluster -- which must name that same hypervisor. A
// machine records where it lives, but not how to get in; a cluster that is gone
// or names another hypervisor leaves it waiting, never falling back.
//
// The HydraCluster is normally found through the owner Machine. During
// teardown, when that Machine is gone, Reconcile has already found it through
// the machine's own cluster-name label instead: see
// withClusterByLabelForDeletion.
//
// A Secret that is missing or broken is a wait here, not a failure. It is the
// HydraCluster's problem, reported terminally there, and an operator may be in
// the middle of replacing it; marking every machine of the cluster
// ProvisioningFailed -- running ones included -- would invite remediation of
// machines that are fine.
func (r *HydraMachineReconciler) hypervisorFor(ctx context.Context, link *linkage, placement string) (*providers.Hypervisor, error) {
	if placement == "" {
		return nil, nil
	}
	hydraCluster := hydraClusterOf(link)
	if hydraCluster == nil || hydraCluster.Spec.Hypervisor == nil {
		return nil, fmt.Errorf("%w: it lives on %s, and its HydraCluster cannot be read or names no hypervisor",
			errWaitingForHypervisorCredentials, placement)
	}
	if named, err := canonicalHypervisor(hydraCluster.Spec.Hypervisor.Address); err != nil || named != placement {
		return nil, fmt.Errorf("%w: it lives on %s, and HydraCluster %s names %s",
			errWaitingForHypervisorCredentials, placement, hydraCluster.Name, hydraCluster.Spec.Hypervisor.Address)
	}
	hypervisor, err := hypervisorOf(ctx, r.APIReader, hydraCluster)
	if errors.Is(err, providers.ErrTerminal) {
		return nil, fmt.Errorf("%w: %v", errWaitingForHypervisorCredentials, err)
	}
	return hypervisor, err
}

// clusterByLabel finds a machine's Cluster, and the HydraCluster that Cluster
// names, through the machine's own cluster-name label, for teardown when the
// owner Machine cannot lead there. Either is nil when it is not there.
func (r *HydraMachineReconciler) clusterByLabel(ctx context.Context, machine *infrav1.HydraMachine) (*clusterv1.Cluster, *infrav1.HydraCluster, error) {
	name := machine.Labels[clusterv1.ClusterNameLabel]
	if name == "" {
		return nil, nil, nil
	}
	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: name}, cluster); err != nil {
		return nil, nil, client.IgnoreNotFound(err)
	}
	ref := cluster.Spec.InfrastructureRef
	if ref.Kind != hydraClusterKind || ref.APIGroup != infrav1.GroupVersion.Group || ref.Name == "" {
		return cluster, nil, nil
	}
	hydraCluster := &infrav1.HydraCluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: ref.Name}, hydraCluster); err != nil {
		return cluster, nil, client.IgnoreNotFound(err)
	}
	return cluster, hydraCluster, nil
}

// withClusterByLabelForDeletion returns link with its Cluster and HydraCluster
// found through the machine's own cluster-name label, for whichever the owner
// Machine could not lead to, when the machine is being deleted. Otherwise link
// unchanged.
//
// The Cluster is what the pause check reads: a paused Cluster must stop this
// teardown however the Cluster was found. The HydraCluster gives a machine
// deleted before its first Create its placement -- the cluster's confirmed
// hypervisor; without it that would be the flag connection, which a cluster
// that left the flags may not reach -- and the storage pool teardown sweeps.
func (r *HydraMachineReconciler) withClusterByLabelForDeletion(ctx context.Context, machine *infrav1.HydraMachine, link *linkage) (*linkage, error) {
	if machine.DeletionTimestamp.IsZero() || (link.cluster != nil && link.hydraCluster != nil) {
		return link, nil
	}
	cluster, hydraCluster, err := r.clusterByLabel(ctx, machine)
	if err != nil {
		return link, err
	}
	found := *link
	if found.cluster == nil {
		found.cluster = cluster
	}
	if found.hydraCluster == nil {
		found.hydraCluster = hydraCluster
	}
	return &found, nil
}

// placementFor decides which hypervisor a machine's operations go to, as
// host:port, or "" for the manager's flag connection.
//
// A machine that records one lives there, whatever its cluster says now. One
// that records none but may have been made was made on the flag connection,
// because a hypervisor is always recorded before Create, in the same patch as
// the storage pool. It may have been made if it has a providerID, or if it
// records a storage pool -- written just before a Create that may have left a
// domain or volumes behind without the providerID ever being recorded.
//
// Only a machine nothing was ever attempted for takes its cluster's
// hypervisor, and only once the cluster has confirmed it; until then a new
// machine waits, while one being deleted looks on the flag connection, where
// it would have been made.
func placementFor(machine *infrav1.HydraMachine, link *linkage) (string, error) {
	if recorded := machine.Annotations[infrav1.HypervisorAnnotation]; recorded != "" {
		return recorded, nil
	}
	if mayBeMadeOnFlags(machine) {
		return "", nil
	}
	// A HydraCluster that cannot be read leaves this on the flag connection, but
	// only for looking: a machine made there whose providerID was never recorded
	// is found and adopted, which must not depend on the cluster, and nothing is
	// created, because clusterReadyForCreate waits for an unreadable HydraCluster.
	hydraCluster := link.hydraCluster
	if hydraCluster == nil || hydraCluster.Spec.Hypervisor == nil {
		return "", nil
	}
	if !confirmedHypervisor(hydraCluster) {
		if !machine.DeletionTimestamp.IsZero() {
			return "", nil
		}
		return "", errWaitingForHypervisor
	}
	return canonicalHypervisor(hydraCluster.Spec.Hypervisor.Address)
}

// mayBeMadeOnFlags reports whether a machine that records no hypervisor may
// have a domain or volumes on the manager's flag connection: it has a
// providerID, or it records a storage pool, which is written just before
// Create. See placementFor.
func mayBeMadeOnFlags(machine *infrav1.HydraMachine) bool {
	return (machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "") ||
		machine.Annotations[infrav1.StoragePoolAnnotation] != ""
}

// machineOwner is how a machine is known to the connection holder.
func machineOwner(key types.NamespacedName) providers.Owner {
	return providers.Owner{Kind: hydraMachineKind, NamespacedName: key}
}

// release tells the connection holder a machine no longer uses a connection.
func (r *HydraMachineReconciler) release(ctx context.Context, owner providers.Owner) {
	if r.ReleaseProvider != nil {
		r.ReleaseProvider(ctx, owner)
	}
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;clusters,verbs=get;list;watch
// Secrets are read one at a time through the uncached APIReader, never listed
// or watched -- so this grant is deliberately get-only. Widening it would make
// a cluster-wide Secret informer possible again.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile drives a HydraMachine towards its desired state.
func (r *HydraMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	machine := &infrav1.HydraMachine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		// Not found is normal: the object was deleted and its finalizer already
		// removed. All that is left is its share of a connection.
		if apierrors.IsNotFound(err) {
			r.release(ctx, machineOwner(req.NamespacedName))
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the Cluster API objects around this machine before anything else.
	// Both the paused check and the bootstrap data depend on them. A machine that
	// declares itself standalone needs none of them; one that merely has no owner
	// reference yet waits for the owner Cluster API is about to attach.
	link, err := r.resolveLinkage(ctx, machine)
	if err != nil {
		if statusErr := r.recordError(ctx, machine, "Provisioning", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	// Deleting is the one case where the Cluster and HydraCluster may be needed
	// though the owner Machine cannot lead there -- its finalizer removed by
	// hand, or its name reused -- so teardown finds them by the machine's own
	// cluster-name label instead. Before the pause check, which has to see a
	// paused Cluster found that way too. Creation keeps its gates: only
	// teardown looks the cluster up by label.
	if link, err = r.withClusterByLabelForDeletion(ctx, machine, link); err != nil {
		if statusErr := r.recordError(ctx, machine, "Deleting", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	// Cluster API's pause signals mean "do not touch this object". That covers
	// deletion as well as creation: an operator pausing a machine to investigate
	// it would not expect the controller to keep destroying infrastructure
	// underneath them. The finalizer stays, so deletion resumes once the pause is
	// lifted -- and the Cluster watch is what notices that it has been.
	if reason := pausedReason(machine.Annotations, link.cluster); reason != "" {
		log.V(1).Info("Reconciliation is paused", "name", machine.Name, "reason", reason)
		return ctrl.Result{}, r.setPaused(ctx, machine, reason)
	}
	if err := r.setPaused(ctx, machine, ""); err != nil {
		return ctrl.Result{}, err
	}

	placement, err := placementFor(machine, link)
	var prov providers.MachineProvider
	if err == nil {
		prov, err = r.provider(ctx, machine, link, placement)
	}
	if reason := waitReasonFor(err); reason != "" {
		if statusErr := r.recordWaiting(ctx, machine, reason, err.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueWhileProvisioning}, nil
	}
	if err != nil {
		phase := "Provisioning"
		if !machine.DeletionTimestamp.IsZero() {
			phase = "Deleting"
		}
		if statusErr := r.recordError(ctx, machine, phase, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if !machine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, prov, machine, link)
	}
	log.V(1).Info("Reconciling machine", "name", machine.Name)
	return r.reconcileNormal(ctx, prov, machine, link, placement)
}

func (r *HydraMachineReconciler) reconcileNormal(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine, link *linkage, placement string) (ctrl.Result, error) {
	// The finalizer must be persisted before any infrastructure exists.
	// Registering it afterwards leaves a window where a delete would remove the
	// object and strand the VM.
	if !controllerutil.ContainsFinalizer(machine, MachineFinalizer) {
		patch := client.MergeFrom(machine.DeepCopy())
		controllerutil.AddFinalizer(machine, MachineFinalizer)
		if err := r.Patch(ctx, machine, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	state, err := r.ensureMachine(ctx, prov, machine, link, placement)
	if reason := waitReasonFor(err); reason != "" {
		// Not a failure. Cluster API populates a machine's owner, its cluster's
		// infrastructure and its bootstrap Secret asynchronously, so every machine
		// it creates passes through these states on its way up. Each gets its own
		// reason -- an operator watching a machine that never joins needs to know
		// whether it is waiting on the Machine controller, on the infrastructure
		// cluster, on CABPK, or on the hypervisor, and
		// "ProvisioningFailedRetrying" would answer none of them.
		if statusErr := r.recordWaiting(ctx, machine, reason, err.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueWhileProvisioning}, nil
	}
	if err != nil {
		if statusErr := r.recordError(ctx, machine, "Provisioning", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, machine, state); err != nil {
		return ctrl.Result{}, err
	}

	// Poll while the machine is still coming up, and also while it is running but
	// has not yet reported an address. Addresses arrive from the guest agent or a
	// DHCP lease some time after the domain starts, and this controller watches no
	// secondary resources -- so stopping at Ready would leave the address list
	// permanently empty for most machines. Once addressed, keep a slower health
	// requeue: a later vanished VM would otherwise stay Ready=True until an
	// unrelated event or the default cache resync.
	if !state.Ready || !hasIPAddress(state.Addresses) {
		return ctrl.Result{RequeueAfter: requeueWhileProvisioning}, nil
	}
	return ctrl.Result{RequeueAfter: requeueWhileHealthy}, nil
}

// ensureMachine returns the machine's current state, creating it if it does not
// exist yet.
func (r *HydraMachineReconciler) ensureMachine(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine, link *linkage, placement string) (*providers.MachineState, error) {
	ident, err := identityOf(machine)
	if err != nil {
		return nil, err
	}
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		id, err := r.machineIDFor(prov, *machine.Spec.ProviderID)
		if err != nil {
			return nil, err
		}
		state, err := prov.Get(ctx, id)
		if errors.Is(err, providers.ErrInvalidID) {
			// The recorded ID can never name a machine, so this object will never
			// reconcile. Say so terminally rather than retrying forever.
			return nil, fmt.Errorf("%w: recorded providerID %q is not usable by the %s backend",
				providers.ErrTerminal, *machine.Spec.ProviderID, prov.Name())
		}
		if err == nil {
			if ownErr := r.confirmOwner(ctx, prov, machine, ident, state); ownErr != nil {
				return nil, ownErr
			}
		}
		if err != nil {
			if errors.Is(err, providers.ErrNotFound) {
				// The machine was deleted out from under us. Recreating is not an
				// option: providerID is immutable and a new machine would get a new
				// ID, so the Node association would be wrong. Surface it as terminal
				// so an operator or MachineHealthCheck replaces the object rather
				// than retrying forever.
				return nil, fmt.Errorf("%w: backing machine %q no longer exists; the HydraMachine must be replaced",
					providers.ErrTerminal, id)
			}
			return nil, fmt.Errorf("querying machine %q: %w", id, err)
		}
		return state, nil
	}

	// No providerID yet, which means one of two things: no machine has been made,
	// or one was made and the patch recording its ID failed. Ask before deciding,
	// because only the first case needs bootstrap data.
	//
	// Fetching it unconditionally would make recovery from that crash window
	// depend on the Secret still existing. It may not -- it can be rotated, or
	// cleaned up once the machine is up -- and then this would wait forever on
	// data the machine already consumed, never reaching the idempotent Create
	// that would have rediscovered it. The providerID could never be recorded,
	// for a machine that is running perfectly well.
	//
	// Another object's identity here has no explanation that makes it safe to
	// follow. A moved object always has a providerID -- clusterctl moves a
	// Machine only once it has a Node -- and this object's own crash window
	// records its own UID. What is left is a template's metadata or a hand
	// edit, and honouring it would adopt or build under a name derived from
	// some other object; ignoring it would let the name change the moment a
	// providerID is recorded. So refuse, and say why.
	if ident.lastOwner != "" && ident.lastOwner != string(machine.UID) {
		// Same reasoning for the owner last confirmed: only a move carries
		// another object's, and a move carries a providerID with it.
		return nil, fmt.Errorf("%w: annotation %s names UID %q, not this HydraMachine's, and it has no providerID; "+
			"only the controller may set it -- remove it, and from any template whose metadata carries it",
			providers.ErrTerminal, infrav1.OwnerUIDAnnotation, ident.lastOwner)
	}
	if ident.foreign {
		return nil, fmt.Errorf("%w: annotation %s names UID %q, not this HydraMachine's, and it has no providerID; "+
			"only the controller may set it -- remove it, and from any template whose metadata carries it "+
			"(a standalone HydraMachine must not be moved before it has a providerID)",
			providers.ErrTerminal, infrav1.IdentityUIDAnnotation, ident.uid)
	}

	var spec providers.MachineSpec
	// recordedNote explains a Create failure when the machine is pinned to a
	// recorded pool other than the one that resolves now.
	var recordedNote string
	switch found, findErr := prov.FindByName(ctx, backendName(machine)); {
	case findErr == nil:
		// The name is this object's own, so another owner on record is not a
		// crash window of ours: refuse it rather than adopt it.
		if found.Owner != "" && found.Owner != string(machine.UID) {
			return nil, fmt.Errorf("%w: a machine named %q exists but is recorded as owned by %q, not this HydraMachine",
				providers.ErrOwnerMismatch, found.Name, found.Owner)
		}
		// Already exists. Create adopts it below -- including starting it if a
		// previous attempt defined it without ever starting it.
		//
		// The spec is deliberately name-only. MachineProvider.Create documents
		// that adoption must depend on nothing but spec.Name, and this is the
		// caller that relies on it: resolving the full spec here would make
		// recovery fail whenever the resolved values had since become
		// unavailable -- a deleted or unreadable HydraCluster leaving no
		// networks, say -- and a machine whose providerID was never recorded
		// would then be unadoptable forever, which is a permanently orphaned VM.
		logf.FromContext(ctx).Info("Adopting a machine whose providerID was never recorded",
			"name", backendName(machine))
		spec = providers.MachineSpec{Name: backendName(machine)}
		// Found on a named hypervisor, it has to say so before it gains a
		// providerID: a providerID with no hypervisor recorded means the flag
		// connection, and every later operation -- teardown included -- would
		// go there instead. Only the hypervisor: the storage pool is whatever
		// the domain already uses, and a pool resolved now could be wrong.
		if _, err := r.recordPlacement(ctx, machine, "", placement); err != nil {
			return nil, err
		}
	case errors.Is(findErr, providers.ErrNotFound):
		// Nothing exists, so this really is a create. Every gate belongs here and
		// not earlier: an existing machine being adopted above must not be blocked
		// on the cluster, on a Secret it already consumed, or on values that
		// resolve differently now than when it was built.
		if err := clusterReadyForCreate(link); err != nil {
			return nil, err
		}
		bootstrapData, err := r.bootstrapDataFor(ctx, link)
		if err != nil {
			return nil, err
		}
		if spec, err = specFor(machine, link, bootstrapData); err != nil {
			return nil, err
		}
		// Before Create, not after: a crash between Create allocating volumes
		// and defining the domain is exactly the leftover teardown has to find,
		// and it can only look in the right pool if this was already recorded.
		//
		// And Create then builds in the recorded pool, whatever resolves now. A
		// retry after the manager's default changed would otherwise put volumes
		// in the new default while teardown protects only the recorded one.
		resolved := prov.StoragePoolFor(spec.StoragePool)
		pool, err := r.recordPlacement(ctx, machine, resolved, placement)
		if err != nil {
			return nil, err
		}
		if pool != "" {
			spec.StoragePool = pool
		}
		if pool != resolved {
			// The record wins, which is right while volumes may be there -- but
			// if that pool has since been removed, every Create fails with an
			// error about a pool nobody configured any more. Say where it came
			// from, so the way out (the annotation) is visible.
			recordedNote = fmt.Sprintf(" (building in storage pool %q recorded in annotation %s; "+
				"the cluster now resolves %q -- if no volumes of this machine exist in %q, "+
				"removing the annotation lets it build there)",
				pool, infrav1.StoragePoolAnnotation, resolved, pool)
		}
	default:
		return nil, fmt.Errorf("searching for an existing machine: %w", findErr)
	}

	// Create is idempotent on name, which is what makes this safe: if a previous
	// reconcile created the machine but crashed before persisting the providerID,
	// this call returns that same machine rather than creating a second one.
	state, err := prov.Create(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("creating machine%s: %w", recordedNote, err)
	}

	providerID := providers.ProviderID(prov.Name(), state.ID)
	patch := client.MergeFrom(machine.DeepCopy())
	machine.Spec.ProviderID = &providerID
	if err := r.Patch(ctx, machine, patch); err != nil {
		// The machine exists but its ID is unrecorded. The next reconcile recovers
		// via Create's idempotency, which is why that guarantee is mandatory
		// rather than merely nice.
		return nil, fmt.Errorf("recording providerID %q: %w", providerID, err)
	}
	return state, nil
}

func (r *HydraMachineReconciler) reconcileDelete(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine, link *linkage) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(machine, MachineFinalizer) {
		return ctrl.Result{}, nil
	}

	id, sweep, err := r.deletionTargetFor(ctx, prov, machine)
	if err != nil {
		if statusErr := r.recordError(ctx, machine, "Deleting", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if id != "" {
		if err := prov.Delete(ctx, id); err != nil {
			if statusErr := r.recordError(ctx, machine, "Deleting", err); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, fmt.Errorf("deleting machine %q: %w", id, err)
		}
	}

	// Sweep by name even when the ID delete reported success. Delete succeeds
	// when the domain is already absent, but an externally undefined domain --
	// or a lookup losing a race with one -- leaves the name-keyed qcow2 behind.
	// Releasing the finalizer at that point would orphan the disk with nothing
	// left referencing it. DeleteByName is idempotent, so the common case where
	// everything was already removed costs one no-op lookup.
	//
	// The machine's pool goes with it. It is the one pool the sweep must not
	// give up on, and the only one: a stopped pool elsewhere on the host must not
	// hold every deletion hostage. See storagePoolOf for where it comes from.
	if err := prov.DeleteByName(ctx, sweep, storagePoolOf(machine, link)); err != nil {
		if statusErr := r.recordError(ctx, machine, "Deleting", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, fmt.Errorf("removing residual resources for %q: %w", sweep, err)
	}

	patch := client.MergeFrom(machine.DeepCopy())
	controllerutil.RemoveFinalizer(machine, MachineFinalizer)
	if err := r.Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	// Teardown is done, so this machine needs no connection any more. The
	// reconcile that sees it gone releases too; this one just does not wait.
	r.release(ctx, machineOwner(client.ObjectKeyFromObject(machine)))
	return ctrl.Result{}, nil
}

// deletionTargetFor works out which backend machine, if any, this object owns,
// and the name whose leftovers teardown may sweep.
//
// The providerID is the normal answer. When it is absent the machine may still
// exist: Create can succeed and the patch recording its ID can then fail. Left
// unhandled, deletion would skip cleanup, release the finalizer, and orphan a
// running VM permanently -- so the name is used as a second handle.
//
// Nothing is deleted that this object has not been confirmed to own, on the
// backend's own record (see confirmOwner). A moved object that never took its
// machine over -- deleted while still paused, or a stale copy of one on a
// second management cluster -- is refused rather than allowed to destroy a VM
// the cluster it came from may still run. Undo a move by moving back.
//
// Returns an empty ID when no providerID was persisted, in which case the
// caller must DeleteByName so a volume-only leftover is still removed.
func (r *HydraMachineReconciler) deletionTargetFor(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine) (string, string, error) {
	ident, err := identityOf(machine)
	if err != nil {
		return "", "", err
	}
	own := string(machine.UID)
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		id, err := r.machineIDFor(prov, *machine.Spec.ProviderID)
		if err != nil {
			// An unusable providerID identifies nothing, and blocking deletion
			// forever over it is worse than proceeding. Fall through to the name.
			logf.FromContext(ctx).Error(err, "ProviderID unusable during deletion, falling back to name lookup",
				"providerID", *machine.Spec.ProviderID)
		} else {
			// Confirm the ID actually resolves to this object's machine before
			// anything destructive happens. A providerID supplied at creation time
			// could otherwise point deletion at an unrelated domain.
			state, err := prov.Get(ctx, id)
			switch {
			case err == nil:
				if ownErr := verifyOwnership(machine, ident, state); ownErr != nil {
					return "", "", ownErr
				}
				if ownErr := ownsForDeletion(machine, ident, state); ownErr != nil {
					return "", "", ownErr
				}
				return id, backendNameFor(machine, ident.uid), nil
			case errors.Is(err, providers.ErrNotFound), errors.Is(err, providers.ErrInvalidID):
				// Nothing under that ID, or an ID the backend can never resolve --
				// a providerID like hydra://libvirt/not-a-uuid satisfies the CRD and
				// the format check but names no machine. Either way, fall through to
				// the name lookup, which is scoped to this object by construction.
				// Returning an error instead would wedge the finalizer forever over
				// a value that can never become valid.
			default:
				return "", "", fmt.Errorf("confirming ownership before deletion: %w", err)
			}
		}
	}

	// From here on there is no machine whose owner record could vouch for this
	// object, so sweep only what its own UID names. A moved-from name is
	// another object's until something here confirmed otherwise, and the
	// cluster that object lives on may still have resources under it.
	//
	// Except where this object records that it took the machine over, and the
	// machine is still there to agree. The record is an annotation anyone able
	// to patch the object can write, so it only says where to look: the
	// domain's own owner record decides, through ownsForDeletion. With no
	// domain left there is nothing on the backend to vouch for leftovers under
	// that name, and they are left alone rather than swept on the annotation's
	// word -- an orphaned disk is recoverable, a deleted one is not.
	if ident.foreign {
		moved := backendNameFor(machine, ident.uid)
		if ident.lastOwner == own {
			state, err := prov.FindByName(ctx, moved)
			switch {
			case err == nil:
				if err := ownsForDeletion(machine, ident, state); err != nil {
					return "", "", err
				}
				return state.ID, moved, nil
			case !errors.Is(err, providers.ErrNotFound):
				return "", "", fmt.Errorf("searching for the machine this object was moved with: %w", err)
			}
		}
		logf.FromContext(ctx).Info("Not sweeping resources named for the identity this object was moved with; nothing on the backend confirms it owns them",
			"name", moved)
	}
	sweep := backendName(machine)
	state, err := prov.FindByName(ctx, sweep)
	if err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return "", sweep, nil
		}
		return "", "", fmt.Errorf("searching for an unrecorded machine: %w", err)
	}
	if err := ownsForDeletion(machine, identity{uid: own}, state); err != nil {
		return "", "", err
	}
	logf.FromContext(ctx).Info("Found a machine whose providerID was never recorded, deleting it",
		"name", sweep, "id", state.ID)
	return state.ID, sweep, nil
}

// identity is the UID a machine's backend name derives from, and whether that
// is another object's: one moved here from another management cluster, which
// may or may not have taken its machine over yet. See
// infrav1.IdentityUIDAnnotation.
type identity struct {
	uid     string
	foreign bool
	// lastOwner is the owner the backend last confirmed for this machine, as
	// carried across by a move: this object's own UID once it has taken the
	// machine over, the object it was moved from before that, or empty when
	// nothing was ever confirmed. See infrav1.OwnerUIDAnnotation.
	lastOwner string
}

// canonicalUID is the only shape an object UID takes. Anything else in the
// annotation is refused rather than hashed: the hash input is
// namespace/name/uid, and only a value that cannot contain a slash keeps it
// unambiguous.
var canonicalUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// identityOf is the one place a machine's identity is resolved, so every name
// derived from it -- for creating, adopting, confirming a hypervisor and
// sweeping -- agrees.
func identityOf(machine *infrav1.HydraMachine) (identity, error) {
	id := identity{uid: string(machine.UID)}
	uid, err := recordedUID(machine, infrav1.IdentityUIDAnnotation)
	if err != nil {
		return identity{}, err
	}
	if uid != "" {
		id.uid, id.foreign = uid, uid != string(machine.UID)
	}
	if id.lastOwner, err = recordedUID(machine, infrav1.OwnerUIDAnnotation); err != nil {
		return identity{}, err
	}
	return id, nil
}

// recordedUID reads a UID the controller recorded in an annotation, or "".
// The object's own UID is the API server's and needs no checking; anything
// else must be a UID, or it is refused before any name is derived from it.
func recordedUID(machine *infrav1.HydraMachine, annotation string) (string, error) {
	recorded := machine.Annotations[annotation]
	if recorded == "" || recorded == string(machine.UID) || canonicalUID.MatchString(recorded) {
		return recorded, nil
	}
	return "", fmt.Errorf("%w: annotation %s is %q, which is not an object UID; only the controller may set it",
		providers.ErrTerminal, annotation, recorded)
}

// confirmOwner checks that a machine found under this object's providerID is
// this object's, on the backend's own record, and brings that record up to
// date: it takes over a machine this object was moved with, and records an
// owner on a machine made before owners were recorded. Either way the identity
// annotation is then recorded if it is not yet, which is the migration every
// machine needs before it can be moved.
func (r *HydraMachineReconciler) confirmOwner(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine, ident identity, state *providers.MachineState) error {
	if err := verifyOwnership(machine, ident, state); err != nil {
		return err
	}
	own := string(machine.UID)
	switch {
	case state.Owner == own:
	case state.Owner == "":
		// Nobody on record: a machine made before owners were recorded, or one
		// whose record Create never got to write -- possibly moved since. The
		// name just matched, and claiming from nobody is a compare like any
		// other: of every copy of this object, only the first to claim wins.
		if err := prov.ClaimOwner(ctx, state.ID, "", own); err != nil {
			return fmt.Errorf("recording the owner of machine %q: %w", state.ID, err)
		}
		logf.FromContext(ctx).Info("Recorded owner on a machine that had none recorded",
			"name", state.Name, "id", state.ID)
	case movedFrom(ident, state):
		// Moved here, and not yet taken over: the record still names the object
		// this one was moved from. Only one copy can take it over -- the claim
		// fails for any other once this one succeeds.
		if err := prov.ClaimOwner(ctx, state.ID, state.Owner, own); err != nil {
			return fmt.Errorf("taking over moved machine %q: %w", state.ID, err)
		}
		logf.FromContext(ctx).Info("Took over a machine moved from another management cluster",
			"name", state.Name, "id", state.ID, "from", state.Owner)
	default:
		return ownerMismatch(machine, ident, state)
	}
	return r.recordOwnership(ctx, machine)
}

// movedFrom reports that a machine's recorded owner is the object this one was
// moved from: the owner last confirmed before the move, or -- for an object
// moved before it ever confirmed one -- the object its name was derived from.
func movedFrom(ident identity, state *providers.MachineState) bool {
	return state.Owner != "" && (state.Owner == ident.lastOwner || (ident.foreign && state.Owner == ident.uid))
}

// recordOwnership writes what the next move needs, once the backend's record
// names this object: its identity, if not yet recorded, and itself as the owner
// last confirmed. Optimistic, like recordPlacement.
func (r *HydraMachineReconciler) recordOwnership(ctx context.Context, machine *infrav1.HydraMachine) error {
	own := string(machine.UID)
	writeIdentity := machine.Annotations[infrav1.IdentityUIDAnnotation] == ""
	if !writeIdentity && machine.Annotations[infrav1.OwnerUIDAnnotation] == own {
		return nil
	}
	patch := client.MergeFromWithOptions(machine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if machine.Annotations == nil {
		machine.Annotations = map[string]string{}
	}
	if writeIdentity {
		machine.Annotations[infrav1.IdentityUIDAnnotation] = own
	}
	machine.Annotations[infrav1.OwnerUIDAnnotation] = own
	if err := r.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("recording identity and owner: %w", err)
	}
	return nil
}

// ownsForDeletion is the owner check teardown makes. Stricter than
// confirmOwner's, because it takes nothing over: a machine is this object's to
// delete only if the record names it, or names nobody and the name is its own.
func ownsForDeletion(machine *infrav1.HydraMachine, ident identity, state *providers.MachineState) error {
	own := string(machine.UID)
	if state.Owner == own || (state.Owner == "" && !ident.foreign) {
		return nil
	}
	if state.Owner == "" || movedFrom(ident, state) {
		// Only teardown reaches this, and an object being deleted never takes
		// anything over: it goes to reconcileDelete, never to confirmOwner, and
		// its deletion cannot be undone. So say what is left to do.
		return fmt.Errorf("%w: machine %q was moved here with this HydraMachine but never taken over, so it may "+
			"still belong to the management cluster it came from; this object is being deleted and will neither "+
			"take it over nor release its finalizer. To abandon the object without touching the machine, remove "+
			"finalizer %s; to manage the machine here, let a copy that is not being deleted reconcile; to undo the "+
			"move, move back while the source still owns it",
			providers.ErrTerminal, state.Name, MachineFinalizer)
	}
	return ownerMismatch(machine, ident, state)
}

func ownerMismatch(machine *infrav1.HydraMachine, ident identity, state *providers.MachineState) error {
	recorded := state.Owner
	if recorded == "" {
		recorded = "nobody"
	}
	return fmt.Errorf("%w: machine %q is recorded as owned by %s, not this HydraMachine (UID %s, identity %s)",
		providers.ErrOwnerMismatch, state.Name, recorded, machine.UID, ident.uid)
}

// verifyOwnership refuses to act on a machine this object does not own.
//
// providerID is writable at creation, so anyone who can create a HydraMachine
// can point it at an unrelated machine's ID. Format and backend checks do not
// catch that -- a well-formed UUID for someone else's domain passes both. Only
// comparing the resolved machine's name against this object's derived name
// does, and without it deletion would destroy that unrelated machine.
//
// The name is a necessary check, not a sufficient one: a copy of this object
// elsewhere derives the same name. The owner record settles the rest; see
// confirmOwner and ownsForDeletion.
func verifyOwnership(machine *infrav1.HydraMachine, ident identity, state *providers.MachineState) error {
	want := backendNameFor(machine, ident.uid)
	if state.Name != want {
		return fmt.Errorf("%w: providerID resolves to machine %q, which is not owned by this HydraMachine (expected %q); "+
			"if it was moved from another management cluster before annotation %s was recorded, or a template "+
			"sets that annotation, the name no longer derives from the UID it was made with",
			providers.ErrTerminal, state.Name, want, infrav1.IdentityUIDAnnotation)
	}
	return nil
}

// machineIDFor validates that a providerID belongs to this backend and returns
// its machine ID.
//
// The backend segment is not decoration. Handing another backend's ID to this
// provider could query an unrelated machine that happens to share an ID, or
// report that a perfectly healthy machine has vanished.
func (r *HydraMachineReconciler) machineIDFor(prov providers.MachineProvider, providerID string) (string, error) {
	backend, id, err := providers.ParseProviderID(providerID)
	if err != nil {
		// Immutable once set, so a malformed value can never become valid.
		// Classified terminal for the same reason as a backend mismatch or an ID
		// the backend cannot resolve -- otherwise the object reports
		// ProvisioningFailedRetrying forever over something no retry can fix.
		return "", fmt.Errorf("%w: stored providerID %q is malformed: %v",
			providers.ErrTerminal, providerID, err)
	}
	if backend != prov.Name() {
		return "", fmt.Errorf("%w: providerID %q belongs to backend %q, but this controller runs %q",
			providers.ErrTerminal, providerID, backend, prov.Name())
	}
	return id, nil
}

func (r *HydraMachineReconciler) updateStatus(ctx context.Context, machine *infrav1.HydraMachine, state *providers.MachineState) error {
	patch := client.MergeFrom(machine.DeepCopy())

	// initialization.provisioned is an initialization MILESTONE, not a liveness
	// signal: Cluster API reads it to orchestrate first-time provisioning. Letting
	// it fall back to false when a provisioned machine later stops would regress
	// that orchestration, so it only ever latches forward. Ongoing health is the
	// Ready condition's job.
	if state.Ready {
		provisioned := true
		machine.Status.Initialization.Provisioned = &provisioned
	} else if machine.Status.Initialization.Provisioned == nil {
		provisioned := false
		machine.Status.Initialization.Provisioned = &provisioned
	}

	machine.Status.Addresses = toMachineAddresses(state.Addresses)

	cond := metav1.Condition{
		Type:               infrav1.MachineReadyCondition,
		Status:             metav1.ConditionFalse,
		Reason:             "Provisioning",
		Message:            "waiting for the backing machine to become ready",
		ObservedGeneration: machine.Generation,
	}
	if state.Ready {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Provisioned"
		cond.Message = "backing machine is running"
	}
	apimeta.SetStatusCondition(&machine.Status.Conditions, cond)

	// Any successful provider observation means the previous terminal failure is
	// no longer true, whether or not the machine is ready yet. Waiting for Ready
	// would leave a stale ProvisioningFailed on an object whose configuration an
	// operator has already fixed and which is now provisioning normally -- and
	// that condition is exactly what a MachineHealthCheck might act on. Ongoing
	// readiness is represented separately, by Ready=False.
	apimeta.RemoveStatusCondition(&machine.Status.Conditions, infrav1.MachineProvisioningFailedCondition)

	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}
	return nil
}

// setPaused surfaces whether reconciliation is suspended, and why.
//
// The contract asks providers to report this through a Paused condition, so an
// operator can see why a machine has stopped progressing rather than assuming
// the controller is broken. An empty reason means not paused.
//
// The reason is carried in the message rather than inferred, because there are
// now three ways to pause a machine -- its own annotation, the owning Cluster's
// spec.paused, and the Cluster's annotation -- and "paused" alone would leave an
// operator hunting for which one is in effect.
func (r *HydraMachineReconciler) setPaused(ctx context.Context, machine *infrav1.HydraMachine, reason string) error {
	return setPausedCondition(ctx, r.Client, machine, &machine.Status.Conditions,
		infrav1.MachinePausedCondition, reason)
}

// waitReasonFor maps a wait to the condition reason that names it, or returns
// "" for anything that is not a wait.
func waitReasonFor(err error) string {
	switch {
	case errors.Is(err, errWaitingForOwner):
		return "WaitingForOwnerMachine"
	case errors.Is(err, errWaitingForCluster):
		return "WaitingForCluster"
	case errors.Is(err, errWaitingForClusterInfra):
		return "WaitingForClusterInfrastructure"
	case errors.Is(err, errWaitingForHydraCluster):
		return "WaitingForHydraCluster"
	case errors.Is(err, errWaitingForBootstrap):
		return "WaitingForBootstrapData"
	case errors.Is(err, errWaitingForHypervisor):
		return "WaitingForHypervisor"
	case errors.Is(err, errWaitingForHypervisorCredentials):
		return "WaitingForHypervisorCredentials"
	default:
		return ""
	}
}

// recordWaiting reports that the machine is blocked on something that is
// expected to arrive, as distinct from something that has gone wrong.
//
// It sets Ready=False like a failure would, but never raises ProvisioningFailed
// -- and clears any stale one. That condition is documented as unrecoverable and
// a MachineHealthCheck may act on it, so a machine waiting on its bootstrap
// Secret must not carry it.
func (r *HydraMachineReconciler) recordWaiting(ctx context.Context, machine *infrav1.HydraMachine, reason, message string) error {
	patch := client.MergeFrom(machine.DeepCopy())
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               infrav1.MachineReadyCondition,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: machine.Generation,
	})
	apimeta.RemoveStatusCondition(&machine.Status.Conditions, infrav1.MachineProvisioningFailedCondition)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("recording waiting state: %w", err)
	}
	// Count only persisted waits, using the bounded reasons from waitReasonFor.
	metrics.MachineWaitTotal.WithLabelValues(reason).Inc()
	return nil
}

// recordError puts a provider failure on the object, distinguishing failures
// that will not fix themselves from ones that probably will.
//
// Only terminal failures raise ProvisioningFailed. That condition is documented
// as unrecoverable, and a MachineHealthCheck or an operator may act on it --
// so reporting an unreachable hypervisor that way would invite remediation of a
// machine that was about to be fine. Retryable failures surface through
// Ready=False alone.
func (r *HydraMachineReconciler) recordError(ctx context.Context, machine *infrav1.HydraMachine, phase string, cause error) error {
	terminal := errors.Is(cause, providers.ErrTerminal)

	reason := phase + "Failed"
	if !terminal {
		reason = phase + "FailedRetrying"
	}

	patch := client.MergeFrom(machine.DeepCopy())
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               infrav1.MachineReadyCondition,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: machine.Generation,
	})
	if terminal {
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               infrav1.MachineProvisioningFailedCondition,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            cause.Error(),
			ObservedGeneration: machine.Generation,
		})
	}
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("recording failure: %w", err)
	}
	// Count only persisted failures, classified like backend operation outcomes.
	metrics.MachineFailureTotal.
		WithLabelValues(phase, providers.ClassifyOutcome(cause)).
		Inc()
	return nil
}

// hasIPAddress reports whether the backend has told us where the machine is.
//
// Hostnames are synthesised from the machine name and say nothing about
// reachability, so only routable addresses count.
func hasIPAddress(addrs []providers.Address) bool {
	for _, a := range addrs {
		if a.Type == providers.AddressTypeInternalIP || a.Type == providers.AddressTypeExternalIP {
			return true
		}
	}
	return false
}

// backendName is the name this object gives a machine it creates: the one its
// own UID derives. A machine it was moved with may have another; see
// identityOf.
func backendName(machine *infrav1.HydraMachine) string {
	return backendNameFor(machine, string(machine.UID))
}

// backendNameFor derives the machine's globally unique identity on the backend
// from the object's namespace and name and the UID it was made under.
//
// A libvirt domain namespace is flat, while a Kubernetes object name is unique
// only within its namespace. Keying on the object name alone would let two
// HydraMachines called "worker-1" in different namespaces adopt -- and then
// delete -- each other's VM. The object UID closes that, and also ensures a
// deleted-and-recreated object does not inherit its predecessor's machine.
func backendNameFor(machine *infrav1.HydraMachine, uid string) string {
	// Uniqueness comes from a hash over the full namespace, name and UID -- not
	// from a truncated UID, where 32 bits makes a birthday collision plausible in
	// tens of thousands of objects, and a collision means two HydraMachines
	// adopting and then deleting the same VM.
	//
	// The readable prefix is bounded because the name is not merely a label: it
	// becomes a libvirt domain name and, with ".qcow2" appended, a filename.
	// A namespace may be 63 characters and an object name 253, so concatenating
	// them raw exceeds the 255-byte limit of directory-backed pools and a
	// perfectly valid HydraMachine could never provision.
	sum := sha256.Sum256([]byte(machine.Namespace + "/" + machine.Name + "/" + uid))
	digest := hex.EncodeToString(sum[:])[:20] // 80 bits

	readable := machine.Namespace + "-" + machine.Name
	if len(readable) > maxReadableNamePrefix {
		readable = readable[:maxReadableNamePrefix]
	}
	return fmt.Sprintf("%s-%s", readable, digest)
}

// hostnameFor derives a hostname the guest can actually accept.
//
// A Kubernetes object name may be 253 characters; Linux caps a hostname at 64
// bytes including the terminator, and a DNS label at 63. Passing a long name
// through verbatim does not fail loudly -- cloud-init simply does not set the
// hostname, and the machine keeps the one baked into the base image. Every
// clone of that image then answers to the same name, which is precisely the
// collision Hostname exists to prevent.
//
// Truncation alone would trade that collision for a subtler one, so a digest of
// the full identity is appended whenever the name does not fit. Two machines
// sharing a 55-character prefix still get distinct hostnames.
func hostnameFor(machine *infrav1.HydraMachine) string {
	if len(machine.Name) <= maxHostnameLength {
		return machine.Name
	}

	sum := sha256.Sum256([]byte(machine.Namespace + "/" + machine.Name + "/" + string(machine.UID)))
	digest := hex.EncodeToString(sum[:])[:8]

	// Trim both hyphens and dots at the cut. A Kubernetes name may contain
	// dot-separated DNS labels, so a cut landing just after a dot would produce
	// "prefix.-digest", whose next label begins with a hyphen -- rejected, which
	// puts the machine right back on the base image's shared hostname.
	prefix := strings.TrimRight(machine.Name[:maxHostnameLength-len(digest)-1], "-.")
	if prefix == "" {
		// Not reachable for a name the API server accepted, since those begin with
		// an alphanumeric. Guarded anyway: a bare "-digest" is not a valid label.
		return digest
	}
	return prefix + "-" + digest
}

// specFor converts the Kubernetes API object into the backend-neutral spec.
//
// This is the only place quantities are turned into bytes, so no backend has to
// think about units.
//
// bootstrapData comes from the owning Machine's Secret rather than from this
// object, and is passed in rather than read here so that the fetch stays on the
// create path where it belongs.
//
// Image, networks and storage pool are resolved here rather than read straight
// off the object, because each may come from the owning HydraCluster instead.
// Resolution order is machine, then cluster, then the manager's flags -- the
// most specific statement wins, and the flags are the floor.
func specFor(machine *infrav1.HydraMachine, link *linkage, bootstrapData []byte) (providers.MachineSpec, error) {
	networks := resolveNetworks(machine, link)
	if len(networks) == 0 {
		// No fallback exists for this and none should. A machine with no network
		// attachment boots, satisfies every check the backend makes, and can never
		// reach an API server -- healthy-looking and useless, which is the failure
		// mode this provider keeps being reviewed for.
		return providers.MachineSpec{}, fmt.Errorf(
			"%w: no networks on the HydraMachine and none defaulted by its HydraCluster",
			providers.ErrTerminal)
	}

	spec := providers.MachineSpec{
		// Create is reached only for an object building under its own identity
		// (see ensureMachine), so its own UID is both the name and the owner.
		Name:     backendName(machine),
		OwnerUID: string(machine.UID),
		// The object's own name, not the derived backend name. The backend name
		// carries a hash for global uniqueness, which is right for a domain name
		// and wrong for something an operator has to recognise in `kubectl get
		// nodes`. Uniqueness still holds where it matters: Cluster API names
		// Machines uniquely within a namespace, and a cluster's nodes come from
		// one namespace.
		Hostname:      hostnameFor(machine),
		BootstrapData: bootstrapData,
		VCPUs:         machine.Spec.VCPUs,
		MemoryBytes:   machine.Spec.Memory.Value(),
		DiskBytes:     machine.Spec.DiskSize.Value(),
		Image:         resolveImage(machine, link),
		Networks:      networks,
		StoragePool:   resolveStoragePool(link),
		// From the cluster, never the machine. A managed network is the cluster's
		// addressing scheme -- the endpoint every machine in it must not collide
		// with -- so letting one machine opt out would put it on a network its own
		// control plane is not on.
		ManagedNetwork: managedNetworkOf(hydraClusterOf(link)),
	}
	return spec, nil
}

// hydraClusterOf returns the cluster a machine belongs to, or nil.
func hydraClusterOf(link *linkage) *infrav1.HydraCluster {
	if link == nil {
		return nil
	}
	return link.hydraCluster
}

// resolveImage picks the base image: the machine's, else the cluster's, else
// the zero value, which the backend reads as "use the configured default".
func resolveImage(machine *infrav1.HydraMachine, link *linkage) providers.Image {
	if img := machine.Spec.Image; img != nil {
		return providers.Image{Name: img.Name, URL: img.URL, Checksum: img.Checksum}
	}
	if link != nil && link.hydraCluster != nil {
		if img := link.hydraCluster.Spec.BaseImage; img != nil {
			return providers.Image{Name: img.Name, URL: img.URL, Checksum: img.Checksum}
		}
	}
	return providers.Image{}
}

// resolveNetworks picks the network attachments: the machine's, else the
// cluster's. Returning empty is a real answer, and the caller refuses it.
func resolveNetworks(machine *infrav1.HydraMachine, link *linkage) []providers.Network {
	attachments := machine.Spec.Networks
	if len(attachments) == 0 && link != nil && link.hydraCluster != nil {
		attachments = link.hydraCluster.Spec.Networks
	}
	if len(attachments) == 0 {
		return nil
	}
	out := make([]providers.Network, 0, len(attachments))
	for _, n := range attachments {
		out = append(out, providers.Network{Name: n.Name})
	}
	return out
}

// recordPlacement writes where Create is about to build onto the machine --
// the storage pool, and the hypervisor when it is not the manager's flag
// connection -- for whichever is not recorded yet, and returns the recorded
// pool: the one Create must use. The hypervisor needs no such return, because
// placement already came from its record when there was one.
//
// Both are teardown's only way to find what Create may have left: see
// infrav1.StoragePoolAnnotation and infrav1.HypervisorAnnotation.
//
// An existing value is kept, and wins. It was written before an earlier Create
// that may have left volumes in it, and a later attempt resolving a different
// pool -- the manager's default changed -- must neither erase that record nor
// build somewhere else, or the machine's volumes would be split across two
// pools with teardown watching only one.
func (r *HydraMachineReconciler) recordPlacement(ctx context.Context, machine *infrav1.HydraMachine, pool, hypervisor string) (string, error) {
	recordedPool := machine.Annotations[infrav1.StoragePoolAnnotation]
	writePool := recordedPool == "" && pool != ""
	writeHypervisor := hypervisor != "" && machine.Annotations[infrav1.HypervisorAnnotation] == ""
	// The identity goes with the placement, and on its own when that is all
	// that is missing: every caller has either just confirmed this object owns
	// its machine under its own UID or is about to create one under it.
	writeIdentity := machine.Annotations[infrav1.IdentityUIDAnnotation] == "" && machine.UID != ""
	if recordedPool != "" {
		pool = recordedPool
	}
	if !writePool && !writeHypervisor && !writeIdentity {
		return pool, nil
	}
	// Optimistic: a stale cached copy must not overwrite a record another
	// reconcile has just written.
	patch := client.MergeFromWithOptions(machine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if machine.Annotations == nil {
		machine.Annotations = map[string]string{}
	}
	if writePool {
		machine.Annotations[infrav1.StoragePoolAnnotation] = pool
	}
	if writeHypervisor {
		machine.Annotations[infrav1.HypervisorAnnotation] = hypervisor
	}
	if writeIdentity {
		machine.Annotations[infrav1.IdentityUIDAnnotation] = string(machine.UID)
	}
	if err := r.Patch(ctx, machine, patch); err != nil {
		return "", fmt.Errorf("recording storage pool %q, hypervisor %q and identity: %w", pool, hypervisor, err)
	}
	return pool, nil
}

// storagePoolOf is the pool a machine's volumes were created in, for teardown.
//
// The annotation recordStoragePool wrote is the answer. Without it -- a machine
// created before it existed, or one adopted rather than created -- the pool is
// resolved again from the cluster, which is what Create would have used unless
// the HydraCluster is gone or its pool changed since.
func storagePoolOf(machine *infrav1.HydraMachine, link *linkage) string {
	if pool := machine.Annotations[infrav1.StoragePoolAnnotation]; pool != "" {
		return pool
	}
	return resolveStoragePool(link)
}

// resolveStoragePool returns the cluster's pool, or empty for the backend's
// configured default.
//
// There is no per-machine override on purpose: the backend resolves the base
// image as a volume inside this pool, so machines in one cluster sharing one
// pool is what makes a single base image usable by all of them.
func resolveStoragePool(link *linkage) string {
	if link != nil && link.hydraCluster != nil {
		return link.hydraCluster.Spec.StoragePool
	}
	return ""
}

// toMachineAddresses converts, deduplicates and caps the provider's addresses.
//
// The cap is not cosmetic. status.addresses has MaxItems=32 in the CRD, and the
// provider result is unbounded -- a multi-homed guest reporting more than that
// would make every status patch rejected by the API server, after its VM had
// already been created. The machine would exist and be permanently unrecordable.
//
// Order is preserved so the result is deterministic and does not churn the
// object on every reconcile.
func toMachineAddresses(addrs []providers.Address) []clusterv1.MachineAddress {
	if len(addrs) == 0 {
		return nil
	}
	seen := make(map[clusterv1.MachineAddress]struct{}, len(addrs))
	out := make([]clusterv1.MachineAddress, 0, min(len(addrs), maxStatusAddresses))
	for _, a := range addrs {
		converted := clusterv1.MachineAddress{
			Type:    clusterv1.MachineAddressType(a.Type),
			Address: a.Address,
		}
		if _, dup := seen[converted]; dup {
			continue
		}
		seen[converted] = struct{}{}
		out = append(out, converted)
		if len(out) == maxStatusAddresses {
			break
		}
	}
	return out
}

// SetupWithManager sets up the controller with the Manager.
//
// The two watches are what make Cluster API integration timely rather than
// merely correct.
//
// Note what is deliberately NOT watched: Secrets. A Secret watch would make
// controller-runtime cache every Secret in the cluster -- every token, every
// TLS key, every application credential -- in a provider that needs one key
// from a handful of them. The Machine watch covers the same event anyway, since
// spec.bootstrap.dataSecretName is what changes when the Secret becomes usable.
//
// Not watching is only half of that guarantee. A cached Get starts the same
// informer a watch would, so bootstrapDataFor reads through APIReader instead;
// the two together are what keep Secrets out of this process.
func (r *HydraMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HydraMachine{}).
		Named("hydramachine").
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(machineToHydraMachine),
		).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToHydraMachines),
		).
		Complete(r)
}
