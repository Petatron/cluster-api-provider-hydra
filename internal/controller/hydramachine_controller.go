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

// requeueWhileProvisioning is how long to wait before re-checking a machine that
// exists but is not yet reporting ready.
const requeueWhileProvisioning = 15 * time.Second

// HydraMachineReconciler reconciles a HydraMachine object
type HydraMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Provider is the infrastructure backend. It is an interface so the
	// reconciler can be tested exhaustively against a fake, and so a second
	// backend requires no controller changes.
	Provider providers.MachineProvider
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
		log.V(1).Info("reconciliation is paused by annotation", "name", machine.Name)
		return ctrl.Result{}, r.setPaused(ctx, machine, true)
	}
	if err := r.setPaused(ctx, machine, false); err != nil {
		return ctrl.Result{}, err
	}

	if !machine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, machine)
	}
	log.V(1).Info("reconciling machine", "name", machine.Name)
	return r.reconcileNormal(ctx, machine)
}

func (r *HydraMachineReconciler) reconcileNormal(ctx context.Context, machine *infrav1.HydraMachine) (ctrl.Result, error) {
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

	state, err := r.ensureMachine(ctx, machine)
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
	// permanently empty for most machines.
	if !state.Ready || !hasIPAddress(state.Addresses) {
		return ctrl.Result{RequeueAfter: requeueWhileProvisioning}, nil
	}
	return ctrl.Result{}, nil
}

// ensureMachine returns the machine's current state, creating it if it does not
// exist yet.
func (r *HydraMachineReconciler) ensureMachine(ctx context.Context, machine *infrav1.HydraMachine) (*providers.MachineState, error) {
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		id, err := r.machineIDFor(*machine.Spec.ProviderID)
		if err != nil {
			return nil, err
		}
		state, err := r.Provider.Get(ctx, id)
		if err != nil {
			if errors.Is(err, providers.ErrNotFound) {
				// The machine was deleted out from under us. Recreating is not an
				// option: providerID is immutable and a new machine would get a new
				// ID, so the Node association would be wrong. Surface it and let a
				// human or a MachineHealthCheck decide to replace the object.
				return nil, fmt.Errorf("backing machine %q no longer exists; the HydraMachine must be replaced", id)
			}
			return nil, fmt.Errorf("querying machine %q: %w", id, err)
		}
		return state, nil
	}

	// No providerID yet. Create is idempotent on name, which is what makes this
	// safe: if a previous reconcile created the machine but crashed before
	// persisting the providerID, this call returns that same machine rather than
	// creating a second one.
	state, err := r.Provider.Create(ctx, specFor(machine))
	if err != nil {
		return nil, fmt.Errorf("creating machine: %w", err)
	}

	providerID := providers.ProviderID(r.Provider.Name(), state.ID)
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

func (r *HydraMachineReconciler) reconcileDelete(ctx context.Context, machine *infrav1.HydraMachine) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(machine, MachineFinalizer) {
		return ctrl.Result{}, nil
	}

	id, err := r.deletionTargetFor(ctx, machine)
	if err != nil {
		if statusErr := r.recordError(ctx, machine, "Deleting", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if id != "" {
		if err := r.Provider.Delete(ctx, id); err != nil {
			if statusErr := r.recordError(ctx, machine, "Deleting", err); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, fmt.Errorf("deleting machine %q: %w", id, err)
		}
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
// Returns an empty ID when there is genuinely nothing to delete.
func (r *HydraMachineReconciler) deletionTargetFor(ctx context.Context, machine *infrav1.HydraMachine) (string, error) {
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		id, err := r.machineIDFor(*machine.Spec.ProviderID)
		if err != nil {
			// An unusable providerID identifies nothing, and blocking deletion
			// forever over it is worse than proceeding. Fall through to the name.
			logf.FromContext(ctx).Error(err, "providerID unusable during deletion; falling back to name lookup",
				"providerID", *machine.Spec.ProviderID)
		} else {
			return id, nil
		}
	}

	state, err := r.Provider.FindByName(ctx, backendName(machine))
	if err != nil {
		if errors.Is(err, providers.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("searching for an unrecorded machine: %w", err)
	}
	logf.FromContext(ctx).Info("found a machine whose providerID was never recorded; deleting it",
		"name", backendName(machine), "id", state.ID)
	return state.ID, nil
}

// machineIDFor validates that a providerID belongs to this backend and returns
// its machine ID.
//
// The backend segment is not decoration. Handing another backend's ID to this
// provider could query an unrelated machine that happens to share an ID, or
// report that a perfectly healthy machine has vanished.
func (r *HydraMachineReconciler) machineIDFor(providerID string) (string, error) {
	backend, id, err := providers.ParseProviderID(providerID)
	if err != nil {
		return "", fmt.Errorf("stored providerID is unusable: %w", err)
	}
	if backend != r.Provider.Name() {
		return "", fmt.Errorf("%w: providerID %q belongs to backend %q, but this controller runs %q",
			providers.ErrTerminal, providerID, backend, r.Provider.Name())
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
		Type:    infrav1.MachineReadyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  "Provisioning",
		Message: "waiting for the backing machine to become ready",
	}
	if state.Ready {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Provisioned"
		cond.Message = "backing machine is running"
	}
	apimeta.SetStatusCondition(&machine.Status.Conditions, cond)

	// A machine that is running but has not yet reported a failure is no longer
	// failing. Leaving a stale terminal condition on the object would keep
	// alarming about something that has since resolved.
	if state.Ready {
		apimeta.RemoveStatusCondition(&machine.Status.Conditions, infrav1.MachineProvisioningFailedCondition)
	}

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
		Type:    infrav1.MachinePausedCondition,
		Status:  metav1.ConditionFalse,
		Reason:  "NotPaused",
		Message: "reconciliation is active",
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
		Type:    infrav1.MachineReadyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: cause.Error(),
	})
	if terminal {
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:    infrav1.MachineProvisioningFailedCondition,
			Status:  metav1.ConditionTrue,
			Reason:  reason,
			Message: cause.Error(),
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
	uid := string(machine.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return fmt.Sprintf("%s-%s-%s", machine.Namespace, machine.Name, uid)
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

func toMachineAddresses(addrs []providers.Address) []clusterv1.MachineAddress {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]clusterv1.MachineAddress, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, clusterv1.MachineAddress{
			Type:    clusterv1.MachineAddressType(a.Type),
			Address: a.Address,
		})
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
