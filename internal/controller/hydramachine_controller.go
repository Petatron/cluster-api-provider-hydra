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
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
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

	// Provider is the infrastructure backend. It is an interface so the
	// reconciler can be tested exhaustively against a fake, and so a second
	// backend requires no controller changes.
	//
	// Tests set this directly. In production it is left nil and built on first
	// use from NewProvider.
	Provider providers.MachineProvider

	// NewProvider builds the backend on first reconcile.
	//
	// Connecting lazily rather than at startup is deliberate. Building it in
	// main() meant a missing storage pool, or a hypervisor that was simply down,
	// crash-looped the manager before its health probes ever came up -- so the
	// operator saw a restart count instead of a message, and the checked-in e2e
	// suite could never observe a Running manager at all. Failing here instead
	// puts the reason on the object as a terminal condition, where it is visible
	// and survives being read later.
	NewProvider func(context.Context) (providers.MachineProvider, error)

	// mu guards lazy construction of Provider.
	mu sync.Mutex
}

// provider returns the backend, building it on first use.
//
// Only a successful construction is cached, so a hypervisor that was unreachable
// at first reconcile is retried rather than remembered as broken.
func (r *HydraMachineReconciler) provider(ctx context.Context) (providers.MachineProvider, error) {
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

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hydramachines/finalizers,verbs=update

// Reconcile drives a HydraMachine towards its desired state.
func (r *HydraMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	machine := &infrav1.HydraMachine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		// Not found is normal: the object was deleted and its finalizer already
		// removed. Nothing left to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Cluster API's paused annotation means "do not touch this object". That
	// covers deletion as well as creation: an operator pausing a machine to
	// investigate it would not expect the controller to keep destroying
	// infrastructure underneath them. The finalizer stays, so deletion resumes
	// once the annotation is removed.
	if _, paused := machine.Annotations[clusterv1.PausedAnnotation]; paused {
		log.V(1).Info("Reconciliation is paused by annotation", "name", machine.Name)
		return ctrl.Result{}, r.setPaused(ctx, machine, true)
	}
	if err := r.setPaused(ctx, machine, false); err != nil {
		return ctrl.Result{}, err
	}

	prov, err := r.provider(ctx)
	if err != nil {
		if statusErr := r.recordError(ctx, machine, "Provisioning", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if !machine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, prov, machine)
	}
	log.V(1).Info("Reconciling machine", "name", machine.Name)
	return r.reconcileNormal(ctx, prov, machine)
}

func (r *HydraMachineReconciler) reconcileNormal(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine) (ctrl.Result, error) {
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

	state, err := r.ensureMachine(ctx, prov, machine)
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
func (r *HydraMachineReconciler) ensureMachine(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine) (*providers.MachineState, error) {
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
			if ownErr := verifyOwnership(machine, state); ownErr != nil {
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

	// No providerID yet. Create is idempotent on name, which is what makes this
	// safe: if a previous reconcile created the machine but crashed before
	// persisting the providerID, this call returns that same machine rather than
	// creating a second one.
	state, err := prov.Create(ctx, specFor(machine))
	if err != nil {
		return nil, fmt.Errorf("creating machine: %w", err)
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

func (r *HydraMachineReconciler) reconcileDelete(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(machine, MachineFinalizer) {
		return ctrl.Result{}, nil
	}

	id, err := r.deletionTargetFor(ctx, prov, machine)
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
	if err := prov.DeleteByName(ctx, backendName(machine)); err != nil {
		if statusErr := r.recordError(ctx, machine, "Deleting", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, fmt.Errorf("removing residual resources for %q: %w", backendName(machine), err)
	}

	patch := client.MergeFrom(machine.DeepCopy())
	controllerutil.RemoveFinalizer(machine, MachineFinalizer)
	if err := r.Patch(ctx, machine, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// deletionTargetFor works out which backend machine, if any, this object owns.
//
// The providerID is the normal answer. When it is absent the machine may still
// exist: Create can succeed and the patch recording its ID can then fail. Left
// unhandled, deletion would skip cleanup, release the finalizer, and orphan a
// running VM permanently -- so the name is used as a second handle.
//
// Returns an empty ID when no providerID was persisted, in which case the
// caller must DeleteByName so a volume-only leftover is still removed.
func (r *HydraMachineReconciler) deletionTargetFor(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine) (string, error) {
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
				if ownErr := verifyOwnership(machine, state); ownErr != nil {
					return "", ownErr
				}
				return id, nil
			case errors.Is(err, providers.ErrNotFound), errors.Is(err, providers.ErrInvalidID):
				// Nothing under that ID, or an ID the backend can never resolve --
				// a providerID like hydra://libvirt/not-a-uuid satisfies the CRD and
				// the format check but names no machine. Either way, fall through to
				// the name lookup, which is scoped to this object by construction.
				// Returning an error instead would wedge the finalizer forever over
				// a value that can never become valid.
			default:
				return "", fmt.Errorf("confirming ownership before deletion: %w", err)
			}
		}
	}

	state, err := prov.FindByName(ctx, backendName(machine))
	if err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("searching for an unrecorded machine: %w", err)
	}
	logf.FromContext(ctx).Info("Found a machine whose providerID was never recorded, deleting it",
		"name", backendName(machine), "id", state.ID)
	return state.ID, nil
}

// verifyOwnership refuses to act on a machine this object does not own.
//
// providerID is writable at creation, so anyone who can create a HydraMachine
// can point it at an unrelated machine's ID. Format and backend checks do not
// catch that -- a well-formed UUID for someone else's domain passes both. Only
// comparing the resolved machine's name against this object's derived name
// does, and without it deletion would destroy that unrelated machine.
func verifyOwnership(machine *infrav1.HydraMachine, state *providers.MachineState) error {
	want := backendName(machine)
	if state.Name != want {
		return fmt.Errorf("%w: providerID resolves to machine %q, which is not owned by this HydraMachine (expected %q)",
			providers.ErrTerminal, state.Name, want)
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

// setPaused surfaces whether reconciliation is suspended.
//
// The contract asks providers to report this through a Paused condition, so an
// operator can see why a machine has stopped progressing rather than assuming
// the controller is broken.
//
// Note this only checks the annotation on the HydraMachine. The contract also
// asks for spec.paused on the owning Cluster, which this provider cannot read
// until Cluster linkage lands in PET-9.
func (r *HydraMachineReconciler) setPaused(ctx context.Context, machine *infrav1.HydraMachine, paused bool) error {
	existing := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachinePausedCondition)
	if !paused && existing == nil {
		// Nothing to clear, and no reason to issue a write on every reconcile.
		return nil
	}
	if existing != nil && (existing.Status == metav1.ConditionTrue) == paused {
		return nil
	}

	cond := metav1.Condition{
		Type:               infrav1.MachinePausedCondition,
		Status:             metav1.ConditionFalse,
		Reason:             "NotPaused",
		Message:            "reconciliation is active",
		ObservedGeneration: machine.Generation,
	}
	if paused {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Paused"
		cond.Message = fmt.Sprintf("reconciliation is suspended by the %s annotation", clusterv1.PausedAnnotation)
	}

	patch := client.MergeFrom(machine.DeepCopy())
	apimeta.SetStatusCondition(&machine.Status.Conditions, cond)
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("recording paused state: %w", err)
	}
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

// backendName derives the machine's globally unique identity on the backend.
//
// A libvirt domain namespace is flat, while a Kubernetes object name is unique
// only within its namespace. Keying on the object name alone would let two
// HydraMachines called "worker-1" in different namespaces adopt -- and then
// delete -- each other's VM. The object UID closes that, and also ensures a
// deleted-and-recreated object does not inherit its predecessor's machine.
func backendName(machine *infrav1.HydraMachine) string {
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
	sum := sha256.Sum256([]byte(machine.Namespace + "/" + machine.Name + "/" + string(machine.UID)))
	digest := hex.EncodeToString(sum[:])[:20] // 80 bits

	readable := machine.Namespace + "-" + machine.Name
	if len(readable) > maxReadableNamePrefix {
		readable = readable[:maxReadableNamePrefix]
	}
	return fmt.Sprintf("%s-%s", readable, digest)
}

// specFor converts the Kubernetes API object into the backend-neutral spec.
//
// This is the only place quantities are turned into bytes, so no backend has to
// think about units.
func specFor(machine *infrav1.HydraMachine) providers.MachineSpec {
	spec := providers.MachineSpec{
		Name:        backendName(machine),
		VCPUs:       machine.Spec.VCPUs,
		MemoryBytes: machine.Spec.Memory.Value(),
		DiskBytes:   machine.Spec.DiskSize.Value(),
		Image: providers.Image{
			Name:     machine.Spec.Image.Name,
			URL:      machine.Spec.Image.URL,
			Checksum: machine.Spec.Image.Checksum,
		},
	}
	for _, n := range machine.Spec.Networks {
		spec.Networks = append(spec.Networks, providers.Network{Name: n.Name})
	}
	return spec
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
func (r *HydraMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HydraMachine{}).
		Named("hydramachine").
		Complete(r)
}
