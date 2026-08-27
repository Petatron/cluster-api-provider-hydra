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
		if statusErr := r.setFailed(ctx, machine, "ProvisioningFailed", err.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, machine, state); err != nil {
		return ctrl.Result{}, err
	}

	if !state.Ready {
		// The machine exists but has not finished coming up. Nothing to react to,
		// so poll rather than spin.
		return ctrl.Result{RequeueAfter: requeueWhileProvisioning}, nil
	}
	return ctrl.Result{}, nil
}

// ensureMachine returns the machine's current state, creating it if it does not
// exist yet.
func (r *HydraMachineReconciler) ensureMachine(ctx context.Context, machine *infrav1.HydraMachine) (*providers.MachineState, error) {
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		_, id, err := providers.ParseProviderID(*machine.Spec.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("stored providerID is unusable: %w", err)
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

	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		_, id, err := providers.ParseProviderID(*machine.Spec.ProviderID)
		if err != nil {
			// An unparseable providerID cannot identify anything to delete. Blocking
			// deletion forever over it would be worse than proceeding, so log by way
			// of the condition and release the object.
			logf.FromContext(ctx).Error(err, "releasing machine with unusable providerID", "providerID", *machine.Spec.ProviderID)
		} else if err := r.Provider.Delete(ctx, id); err != nil {
			if statusErr := r.setFailed(ctx, machine, "DeletionFailed", err.Error()); statusErr != nil {
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

func (r *HydraMachineReconciler) updateStatus(ctx context.Context, machine *infrav1.HydraMachine, state *providers.MachineState) error {
	patch := client.MergeFrom(machine.DeepCopy())

	machine.Status.Initialization.Provisioned = &state.Ready
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

	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}
	return nil
}

// setFailed records a failure on the object.
//
// The v1beta2 contract removed special handling for terminal failures, so a
// condition is the only signal that reaches an operator at all.
func (r *HydraMachineReconciler) setFailed(ctx context.Context, machine *infrav1.HydraMachine, reason, message string) error {
	patch := client.MergeFrom(machine.DeepCopy())
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:    infrav1.MachineReadyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:    infrav1.MachineProvisioningFailedCondition,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	if err := r.Status().Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("recording failure: %w", err)
	}
	return nil
}

// specFor converts the Kubernetes API object into the backend-neutral spec.
//
// This is the only place quantities are turned into bytes, so no backend has to
// think about units.
func specFor(machine *infrav1.HydraMachine) providers.MachineSpec {
	spec := providers.MachineSpec{
		Name:        machine.Name,
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
