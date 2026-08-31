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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// bootstrapDataSecretKey is the key Cluster API's bootstrap contract puts the
// cloud-init user-data under. Every bootstrap provider writes "value"; it is not
// configurable, and reading any other key would silently boot an unconfigured
// machine.
const bootstrapDataSecretKey = "value"

// machineKind and hydraMachineKind are the object kinds this controller matches
// owner references and infrastructure references against.
const (
	machineKind      = "Machine"
	hydraMachineKind = "HydraMachine"
)

// errWaitingForBootstrap reports that the bootstrap provider has not published
// usable data yet.
//
// This is deliberately not a provider failure. CABPK produces its Secret
// asynchronously, so "not there yet" is the normal early state of every machine
// Cluster API creates -- and routing it through recordError would raise
// ProvisioningFailedRetrying on a machine that is proceeding exactly as designed.
var errWaitingForBootstrap = errors.New("waiting for bootstrap data")

// errWaitingForOwner reports that the object names an owning Machine that this
// controller cannot currently see.
//
// Distinct from errWaitingForBootstrap because it is a different wait with a
// different fix, and an operator needs to be able to tell them apart.
var errWaitingForOwner = errors.New("waiting for the owning Machine")

// linkage is the Cluster API context surrounding a HydraMachine.
//
// Both fields may be nil, and nil is a supported state rather than an error:
// a HydraMachine created directly, with no owning Machine, provisions a VM with
// no bootstrap data. That is the documented way to exercise the infrastructure
// half on its own -- the machine boots and never joins a cluster.
type linkage struct {
	// ownerName is the Machine named by an owner reference, whether or not that
	// Machine could be read. Keeping it separate from machine is what lets the
	// controller tell "nothing owns this" from "its owner is not visible right
	// now" -- two situations that must not be treated alike, because the first
	// provisions and the second must wait.
	ownerName string

	machine *clusterv1.Machine
	cluster *clusterv1.Cluster
}

// resolveLinkage walks from the HydraMachine out to the Cluster API objects that
// give it meaning: the owning Machine, and through it the Cluster.
//
// Nothing here is fatal when it is merely absent. The controller has to keep
// working during teardown, when the Machine and Cluster disappear before the
// infrastructure they described, and an error at that point would wedge the
// finalizer -- the failure mode this codebase has already had to fix twice.
func (r *HydraMachineReconciler) resolveLinkage(ctx context.Context, machine *infrav1.HydraMachine) (*linkage, error) {
	log := logf.FromContext(ctx)

	name := ownerMachineName(machine)
	if name == "" {
		return &linkage{}, nil
	}

	owner := &clusterv1.Machine{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: name}, owner); err != nil {
		if apierrors.IsNotFound(err) {
			// Deliberately NOT treated as standalone. An absent owner is usually
			// transient -- a cache that has not caught up, or a Machine mid-delete --
			// and provisioning through it would create a VM with no bootstrap data.
			// That VM is unrepairable: once providerID is persisted the bootstrap
			// Secret is never read again, so it would never join anything.
			//
			// ownerName is retained so callers can wait. Deletion is unaffected,
			// because teardown never asks for bootstrap data.
			log.V(1).Info("Owner Machine is not visible", "machine", name)
			return &linkage{ownerName: name}, nil
		}
		return nil, fmt.Errorf("reading owner Machine %q: %w", name, err)
	}

	link := &linkage{ownerName: name, machine: owner}

	clusterName := owner.Labels[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		clusterName = machine.Labels[clusterv1.ClusterNameLabel]
	}
	if clusterName == "" {
		return link, nil
	}

	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: clusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// A named but absent Cluster only costs us the spec.paused check. That
			// is worth trading away: a Cluster is deleted while its Machines are
			// still being torn down, so failing here would block exactly the
			// deletions that must still run.
			log.V(1).Info("Cluster named by label does not exist", "cluster", clusterName)
			return link, nil
		}
		return nil, fmt.Errorf("reading Cluster %q: %w", clusterName, err)
	}
	link.cluster = cluster
	return link, nil
}

// ownerMachineName returns the name of the Cluster API Machine owning this
// object, or "" when nothing does.
//
// This is deliberately a local implementation rather than sigs.k8s.io/cluster-api's
// util.GetOwnerMachine. That helper lives in the main Cluster API module, which
// this provider does not depend on -- only the much smaller api module. Pulling
// in the full module to walk a slice of owner references would grow the
// dependency graph considerably for fifteen lines of logic whose contract, an
// owner reference of kind Machine in the cluster.x-k8s.io group, is stable.
func ownerMachineName(machine *infrav1.HydraMachine) string {
	for _, ref := range machine.OwnerReferences {
		if ref.Kind != machineKind {
			continue
		}
		// Compare the group only. Matching the full apiVersion would break on
		// every contract version bump, and the object's own CRD already pins which
		// versions exist.
		// A core-group apiVersion such as "v1" carries no slash, and Cut reports
		// that as not-found -- correct here, since Machine is never core.
		if group, _, found := strings.Cut(ref.APIVersion, "/"); found && group == clusterv1.GroupVersion.Group {
			return ref.Name
		}
	}
	return ""
}

// pausedReason explains why reconciliation is suspended, or returns "".
//
// PET-8 could only see the annotation on the HydraMachine itself. The contract
// also asks providers to honour the owning Cluster, which matters because
// pausing a Cluster is how an operator stops a whole cluster's reconciliation --
// and a provider that ignored it would carry on creating and destroying VMs
// underneath someone who believed they had stopped everything.
func pausedReason(machine *infrav1.HydraMachine, link *linkage) string {
	if _, ok := machine.Annotations[clusterv1.PausedAnnotation]; ok {
		return fmt.Sprintf("the %s annotation", clusterv1.PausedAnnotation)
	}
	if link == nil || link.cluster == nil {
		return ""
	}
	if link.cluster.Spec.Paused != nil && *link.cluster.Spec.Paused {
		return fmt.Sprintf("spec.paused on Cluster %q", link.cluster.Name)
	}
	if _, ok := link.cluster.Annotations[clusterv1.PausedAnnotation]; ok {
		return fmt.Sprintf("the %s annotation on Cluster %q", clusterv1.PausedAnnotation, link.cluster.Name)
	}
	return ""
}

// bootstrapDataFor fetches the cloud-init user-data the machine must boot with.
//
// The path is fixed by the Cluster API bootstrap contract: the owning Machine
// names a Secret, and the Secret's "value" key holds the user-data. The
// infrastructure object never carries it, which is what keeps a join token out
// of a spec that anyone with read access to the CRD can see.
func (r *HydraMachineReconciler) bootstrapDataFor(ctx context.Context, link *linkage) ([]byte, error) {
	if link.machine == nil {
		if link.ownerName != "" {
			// Something owns this object; we just cannot see it yet. Waiting is the
			// only safe answer -- see resolveLinkage.
			return nil, fmt.Errorf("%w: owner Machine %q is not visible yet", errWaitingForOwner, link.ownerName)
		}
		// Nothing owns this object, so there is no bootstrap provider to ask.
		// Documented on providers.MachineSpec as a useful state: the VM boots,
		// configures nothing, and never joins a cluster.
		return nil, nil
	}

	name := link.machine.Spec.Bootstrap.DataSecretName
	if name == nil || *name == "" {
		return nil, fmt.Errorf("%w: Machine %q has no bootstrap data secret yet",
			errWaitingForBootstrap, link.machine.Name)
	}

	// Read through the uncached APIReader, NOT the cached client.
	//
	// This is the whole reason the provider does not end up holding every Secret
	// in the cluster. controller-runtime's client is cache-backed, and the first
	// cached Get for a kind starts an informer for that kind cluster-wide -- so a
	// single r.Get here would quietly retain every token, TLS key and application
	// credential in memory, which is exactly what not watching Secrets was meant
	// to avoid. An uncached read costs one API call per machine creation, once.
	// It is also why the RBAC rule for secrets is get-only.
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: link.machine.Namespace, Name: *name}
	if err := r.APIReader.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Usually a race: the Machine is updated fractionally before the Secret
			// is visible in this controller's cache. Waiting is right either way --
			// if it never appears, the Ready condition says exactly which Secret is
			// missing, which is more useful than a terminal failure that no retry
			// could have been wrong about.
			return nil, fmt.Errorf("%w: bootstrap data secret %q does not exist yet", errWaitingForBootstrap, *name)
		}
		return nil, fmt.Errorf("reading bootstrap data secret %q: %w", *name, err)
	}

	value, ok := secret.Data[bootstrapDataSecretKey]
	if !ok {
		// A Secret that exists but has the wrong shape is a configuration error,
		// not a race. Retrying cannot add the key, and a machine created without
		// user-data would come up unconfigured and never join -- failing loudly
		// beats provisioning something silently useless.
		return nil, fmt.Errorf("%w: bootstrap data secret %q has no %q key",
			providers.ErrTerminal, *name, bootstrapDataSecretKey)
	}
	if len(value) == 0 {
		return nil, fmt.Errorf("%w: bootstrap data secret %q has an empty %q key",
			providers.ErrTerminal, *name, bootstrapDataSecretKey)
	}
	return value, nil
}

// machineToHydraMachine maps a Cluster API Machine to the HydraMachine it points
// at.
//
// Without this watch, the moment that matters most -- the bootstrap provider
// publishing its Secret and the Machine controller recording its name -- is
// invisible to this controller, and every machine in the cluster would sit idle
// until the next 15-second provisioning tick.
func machineToHydraMachine(_ context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok {
		return nil
	}
	ref := machine.Spec.InfrastructureRef
	if ref.Kind != hydraMachineKind || ref.APIGroup != infrav1.GroupVersion.Group || ref.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: machine.Namespace,
		Name:      ref.Name,
	}}}
}

// clusterToHydraMachines maps a Cluster to every HydraMachine belonging to it.
//
// This exists for un-pausing. A paused machine returns without requeueing -- it
// has to, or pausing would still burn a reconcile every few seconds -- so
// nothing would notice spec.paused going back to false. Clusters are few and
// change rarely, so the list is cheap.
func (r *HydraMachineReconciler) clusterToHydraMachines(ctx context.Context, obj client.Object) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}
	machines := &infrav1.HydraMachineList{}
	if err := r.List(ctx, machines,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name},
	); err != nil {
		logf.FromContext(ctx).Error(err, "Listing HydraMachines for a Cluster event", "cluster", cluster.Name)
		return nil
	}
	out := make([]reconcile.Request, 0, len(machines.Items))
	for i := range machines.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&machines.Items[i])})
	}
	return out
}
