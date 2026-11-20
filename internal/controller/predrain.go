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
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// preDrainHookSlack is how long past a hook's own timeout a deleting Machine
// may be held, to cover a run that could not start (an unreachable hypervisor
// or guest agent). After it the hook is released, run or not.
const preDrainHookSlack = 60 * time.Second

// preDrainHookPoll is how often a running hook is checked, and how soon a
// start or status call that failed is tried again.
const preDrainHookPoll = time.Second

// preDrainHookRetry is how soon a failed start or status call is retried.
const preDrainHookRetry = 5 * time.Second

// guestCallTimeout bounds one start or status call to the guest.
const guestCallTimeout = 20 * time.Second

// maxHookOutput bounds how much of a hook's output is logged.
const maxHookOutput = 2048

// PreDrainHookRunAnnotation records, on the HydraMachine, the run of the
// pre-drain hook this controller started: "<Machine UID>/<deletion time,
// Unix seconds>/<guest pid>", or ".../starting" while the start is under way.
// The first two fields tie it to one deletion of one Machine, so a record
// copied by clusterctl move, or left from anything else, is never mistaken
// for this deletion's run.
const PreDrainHookRunAnnotation = "hydramachine.infrastructure.cluster.x-k8s.io/pre-drain-hook-run"

const runStarting = "starting"

// reconcilePreDrainHook serves spec.preDrainHook: it holds the owner Machine's
// deletion with a Cluster API pre-drain hook, runs the program in the guest
// once the Machine starts deleting, and releases the hook when the program
// exits or its time runs out. It returns how soon to look again, or zero to
// carry on with the reconcile.
//
// prov is nil when the hypervisor could not be resolved; only the rules that
// release a hook without running it apply then.
//
// The decision table (PET-88):
//
//	Machine not deleting:
//	  HydraMachine deleting, hook present        -> remove it
//	  hook wanted and absent, finalizer present  -> add it
//	  not wanted and present                     -> remove it (nobody would serve it)
//	  a run record is left over                  -> remove the record
//	Machine deleting, hook present:
//	  HydraMachine deleting                      -> release (Cluster API skipped the hook)
//	  preDrainHook unset                         -> release
//	  Cluster deleting                           -> release (Cluster API skips pre-drain hooks)
//	  past timeout + slack since deletion        -> release, run or not
//	  no providerID, machine gone or not running -> release (nothing to run)
//	  no run recorded, past timeout              -> release (too late to start)
//	  no run recorded                            -> record "starting", start, record pid, poll
//	  "starting" recorded, no pid, within timeout -> hold (it may be running; never start twice)
//	  "starting" recorded, no pid, past timeout  -> release
//	  the guest refused (no program, pid unknown) -> release
//	  start or status call failed                -> retry on a timer, within the bound above
//	  no provider (backend unreachable)          -> retry on a timer, within the bound above
//	  still running, past timeout                -> release (the program is not stopped)
//	  still running                              -> poll
//	  exited, any code                           -> release
//
// The program may be cut off by a reboot or still be running when the hook is
// released, so it must be idempotent and safe to leave running; it is never
// started twice for one deletion.
func (r *HydraMachineReconciler) reconcilePreDrainHook(ctx context.Context, machine *infrav1.HydraMachine, link *linkage, prov providers.MachineProvider) (time.Duration, error) {
	m := link.machine
	if m == nil {
		return 0, nil
	}
	_, held := m.Annotations[infrav1.PreDrainHookAnnotation]
	hook := machine.Spec.PreDrainHook
	log := logf.FromContext(ctx).WithValues("machine", m.Name)

	if m.DeletionTimestamp.IsZero() {
		return 0, r.holdPreDrainHook(ctx, machine, m, held)
	}
	if !held {
		return 0, nil
	}
	release := func(msg string, kv ...any) (time.Duration, error) {
		log.Info(msg, kv...)
		return 0, r.setPreDrainHook(ctx, m, false)
	}

	deleting := time.Since(m.DeletionTimestamp.Time)
	switch {
	case !machine.DeletionTimestamp.IsZero():
		// Cluster API deletes the infrastructure only after the pre-drain
		// stage, so a hook still held here is one it skipped: no Node, the
		// last control-plane node, or the control plane being deleted.
		return release("Released pre-drain hook without running it: Cluster API skipped it")
	case hook == nil:
		return release("Released pre-drain hook without running it: preDrainHook is not set")
	case link.cluster != nil && !link.cluster.DeletionTimestamp.IsZero():
		return release("Released pre-drain hook without running it: the Cluster is being deleted")
	case link.cluster == nil && link.clusterName != "":
		// Named but gone (resolveLinkage leaves a Cluster that is not found
		// as nil and fails on any other read error): its teardown has begun.
		return release("Released pre-drain hook without running it: the Cluster is gone")
	case deleting > hook.Timeout()+preDrainHookSlack:
		return release("Released pre-drain hook: its time ran out", "timeout", hook.Timeout(), "slack", preDrainHookSlack)
	case prov == nil:
		// Come back on a short timer, not through the caller's error path:
		// its backoff grows past the bound above. The backend error is
		// reported again once the hook is released.
		log.Info("Pre-drain hook is waiting for the infrastructure backend")
		return min(preDrainHookRetry, hook.Timeout()+preDrainHookSlack-deleting+time.Second), nil
	case machine.Spec.ProviderID == nil:
		return release("Released pre-drain hook without running it: the machine has no providerID")
	}
	id, err := r.machineIDFor(prov, *machine.Spec.ProviderID)
	if err != nil {
		return release("Released pre-drain hook without running it", "reason", err.Error())
	}
	return r.runPreDrainHook(ctx, machine, m, prov, id, deleting, release)
}

// holdPreDrainHook keeps the hook on a Machine that is not deleting as
// spec.preDrainHook asks, and clears a run record left from anything else.
func (r *HydraMachineReconciler) holdPreDrainHook(ctx context.Context, machine *infrav1.HydraMachine, m *clusterv1.Machine, held bool) error {
	hook := machine.Spec.PreDrainHook
	log := logf.FromContext(ctx).WithValues("machine", m.Name)
	switch {
	case held && !machine.DeletionTimestamp.IsZero():
		log.Info("Removed pre-drain hook from Machine: the HydraMachine is being deleted")
		return r.setPreDrainHook(ctx, m, false)
	case hook != nil && !held && machine.DeletionTimestamp.IsZero() && controllerutil.ContainsFinalizer(machine, MachineFinalizer):
		// Only once the finalizer is in place: a HydraMachine deleted without
		// it would never come back to release the hook.
		return r.setPreDrainHook(ctx, m, true)
	case hook == nil && held:
		log.Info("Removed pre-drain hook from Machine: preDrainHook is not set")
		return r.setPreDrainHook(ctx, m, false)
	}
	if _, recorded := machine.Annotations[PreDrainHookRunAnnotation]; recorded {
		return r.setRunRecord(ctx, machine, "", false)
	}
	return nil
}

// runPreDrainHook starts the program for this deletion, or follows the run
// already started, and releases the hook once it is over.
func (r *HydraMachineReconciler) runPreDrainHook(ctx context.Context, machine *infrav1.HydraMachine, m *clusterv1.Machine, prov providers.MachineProvider, id string, deleting time.Duration, release func(string, ...any) (time.Duration, error)) (time.Duration, error) {
	hook := machine.Spec.PreDrainHook
	log := logf.FromContext(ctx).WithValues("machine", m.Name)
	retry := func(what string, err error) time.Duration {
		log.Info("Pre-drain hook call failed; retrying", "call", what, "path", hook.Path, "reason", err.Error())
		return min(preDrainHookRetry, hook.Timeout()+preDrainHookSlack-deleting+time.Second)
	}

	thisRun := fmt.Sprintf("%s/%d/", m.UID, m.DeletionTimestamp.Unix())
	record := strings.TrimPrefix(machine.Annotations[PreDrainHookRunAnnotation], thisRun)
	if !strings.HasPrefix(machine.Annotations[PreDrainHookRunAnnotation], thisRun) {
		record = ""
	}

	// No call may outlast the hook's hard bound.
	callCtx, cancel := context.WithTimeout(ctx, min(guestCallTimeout, hook.Timeout()+preDrainHookSlack-deleting))
	defer cancel()
	switch {
	case record == "" && deleting > hook.Timeout():
		return release("Released pre-drain hook without running it: too late to start it", "timeout", hook.Timeout())
	case record == "":
		// The same check teardown makes before touching a machine: a
		// providerID can be written at creation, so it must resolve to this
		// object's own machine before anything runs in it as root.
		if reason, ok := r.ownsGuest(callCtx, prov, machine, id); !ok {
			if reason == "" {
				return retry("ownership check", errors.New("the machine could not be read")), nil
			}
			return release("Released pre-drain hook without running it: the machine is not this HydraMachine's", "reason", reason)
		}
		// Recorded before the start, under an optimistic lock: a second
		// reconcile reading a stale object fails here instead of starting a
		// second copy.
		if err := r.setRunRecord(ctx, machine, thisRun+runStarting, true); err != nil {
			return 0, err
		}
		pid, err := prov.StartInGuest(callCtx, id, hook.Path)
		switch {
		case errors.Is(err, providers.ErrGuestOutcomeUnknown):
			// The guest may have started it: the record stays "starting",
			// so the run is held until the timeout and never started again.
			log.Info("Pre-drain hook start has an unknown outcome; holding until its timeout", "path", hook.Path, "reason", err.Error())
			return min(preDrainHookRetry, hook.Timeout()+preDrainHookSlack-deleting+time.Second), nil
		case errors.Is(err, providers.ErrNotFound), errors.Is(err, providers.ErrNotRunning):
			return release("Released pre-drain hook without running it: the machine is not running", "reason", err.Error())
		case errors.Is(err, providers.ErrGuestRefused):
			return release("Released pre-drain hook: the guest could not run it", "path", hook.Path, "reason", err.Error())
		case err == nil && pid <= 0:
			err = fmt.Errorf("the guest reported pid %d", pid)
			fallthrough
		case err != nil:
			// Not started: clear the marker so a retry may start it. Should
			// even that fail, the marker stays and the run is treated as
			// unknown below: held until the timeout, never started again.
			if clearErr := r.setRunRecordRetrying(ctx, machine, ""); clearErr != nil {
				return 0, clearErr
			}
			return retry("start", err), nil
		}
		log.Info("Started pre-drain hook", "path", hook.Path, "pid", pid)
		if err := r.setRunRecordRetrying(ctx, machine, thisRun+strconv.Itoa(pid)); err != nil {
			// The program is running and its pid is lost: the record
			// stays "starting", and the hook is held until the timeout.
			return 0, err
		}
		return preDrainHookPoll, nil
	case record == runStarting && deleting <= hook.Timeout():
		// A start whose outcome this controller does not know -- it
		// crashed, or a record could not be written. The program may be
		// running: hold the drain until the timeout, and never start a
		// second copy.
		log.V(1).Info("Pre-drain hook run is unknown; holding until its timeout", "timeout", hook.Timeout())
		return min(preDrainHookRetry, hook.Timeout()-deleting+time.Second), nil
	case record == runStarting:
		return release("Released pre-drain hook: a start's outcome was never recorded, and its time is up; it is not started twice")
	}
	pid, err := strconv.Atoi(record)
	if err != nil || pid <= 0 {
		return release("Released pre-drain hook: its run record is not valid", "record", record)
	}

	run, err := prov.GuestStatus(callCtx, id, pid)
	switch {
	case errors.Is(err, providers.ErrNotFound), errors.Is(err, providers.ErrNotRunning):
		return release("Released pre-drain hook: the machine stopped running", "reason", err.Error())
	case errors.Is(err, providers.ErrGuestRefused):
		return release("Released pre-drain hook: the guest no longer knows the run", "pid", pid, "reason", err.Error())
	case err != nil:
		return retry("status", err), nil
	case run == nil && deleting > hook.Timeout():
		return release("Pre-drain hook did not finish in time; it is left running and the deletion goes on", "path", hook.Path, "pid", pid, "timeout", hook.Timeout())
	case run == nil:
		return preDrainHookPoll, nil
	}
	log.V(1).Info("Pre-drain hook output", "path", hook.Path, "output", truncate(run.Output))
	if run.ExitCode == 0 {
		return release("Ran pre-drain hook", "path", hook.Path)
	}
	return release("Pre-drain hook failed; the deletion goes on", "path", hook.Path, "exitCode", run.ExitCode)
}

// ownsGuest reports whether the machine id resolves to is this object's, by
// the checks teardown uses: its name derives from this object, and its owner
// record is this object (or unset, for one not moved here). A false with an
// empty reason means the machine could not be read.
func (r *HydraMachineReconciler) ownsGuest(ctx context.Context, prov providers.MachineProvider, machine *infrav1.HydraMachine, id string) (string, bool) {
	ident, err := identityOf(machine)
	if err != nil {
		return err.Error(), false
	}
	state, err := prov.Get(ctx, id)
	switch {
	case errors.Is(err, providers.ErrNotFound):
		return "the machine does not exist", false
	case err != nil:
		return "", false
	}
	if err := verifyOwnership(machine, ident, state); err != nil {
		return err.Error(), false
	}
	if err := ownsForDeletion(machine, ident, state); err != nil {
		return err.Error(), false
	}
	return "", true
}

// setRunRecordRetrying is setRunRecord against a fresh read, tried a few
// times: a run record that cannot be written turns a known outcome into an
// unknown one.
func (r *HydraMachineReconciler) setRunRecordRetrying(ctx context.Context, machine *infrav1.HydraMachine, value string) error {
	var err error
	for attempt := range 4 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
			fresh := &infrav1.HydraMachine{}
			if getErr := r.APIReader.Get(ctx, client.ObjectKeyFromObject(machine), fresh); getErr != nil {
				err = getErr
				continue
			}
			*machine = *fresh
		}
		if err = r.setRunRecord(ctx, machine, value, false); err == nil {
			return nil
		}
	}
	return err
}

// setRunRecord sets PreDrainHookRunAnnotation on machine to value, or removes
// it when value is empty. With lock, the patch fails if machine is stale.
func (r *HydraMachineReconciler) setRunRecord(ctx context.Context, machine *infrav1.HydraMachine, value string, lock bool) error {
	var patch client.Patch
	if lock {
		patch = client.MergeFromWithOptions(machine.DeepCopy(), client.MergeFromWithOptimisticLock{})
	} else {
		patch = client.MergeFrom(machine.DeepCopy())
	}
	if value == "" {
		delete(machine.Annotations, PreDrainHookRunAnnotation)
	} else {
		if machine.Annotations == nil {
			machine.Annotations = map[string]string{}
		}
		machine.Annotations[PreDrainHookRunAnnotation] = value
	}
	if err := r.Patch(ctx, machine, patch); err != nil {
		return fmt.Errorf("recording the pre-drain hook run: %w", err)
	}
	return nil
}

// setPreDrainHook adds or removes this controller's pre-drain hook on m.
func (r *HydraMachineReconciler) setPreDrainHook(ctx context.Context, m *clusterv1.Machine, held bool) error {
	patch := client.MergeFrom(m.DeepCopy())
	if held {
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations[infrav1.PreDrainHookAnnotation] = infrav1.PreDrainHookOwner
	} else {
		delete(m.Annotations, infrav1.PreDrainHookAnnotation)
	}
	if err := r.Patch(ctx, m, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("updating the pre-drain hook on Machine %s: %w", m.Name, err)
	}
	return nil
}

func truncate(s string) string {
	if len(s) <= maxHookOutput {
		return s
	}
	return s[:maxHookOutput] + "…"
}
