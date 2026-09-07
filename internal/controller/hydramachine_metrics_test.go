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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/metrics"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

func TestMachineMetricsRequirePersistedStatus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wait  error
		cause error
	}{
		{name: "owner wait", wait: errWaitingForOwner},
		{name: "cluster wait", wait: errWaitingForCluster},
		{name: "infrastructure wait", wait: errWaitingForClusterInfra},
		{name: "hydra cluster wait", wait: errWaitingForHydraCluster},
		{name: "bootstrap wait", wait: errWaitingForBootstrap},
		{name: "retryable failure", cause: errors.New("connection refused")},
		{name: "terminal failure", cause: providers.ErrTerminal},
		{name: "not found failure", cause: providers.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := infrav1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			machine := &infrav1.HydraMachine{ObjectMeta: metav1.ObjectMeta{
				Name: "metrics-worker", Namespace: "default", Generation: 1,
			}}
			patchErr := errors.New("status patch failed")
			failPatch := true
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(machine).WithObjects(machine).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, subresource string,
						obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						if subresource == "status" && failPatch {
							return patchErr
						}
						return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
					},
				}).Build()
			r := &HydraMachineReconciler{Client: c}
			var counter prometheus.Counter
			var reason string
			record := func() error {
				return r.recordError(t.Context(), machine, "Create", tc.cause)
			}
			if tc.wait != nil {
				reason = waitReasonFor(tc.wait)
				counter = metrics.MachineWaitTotal.WithLabelValues(reason)
				record = func() error {
					return r.recordWaiting(t.Context(), machine, reason, tc.wait.Error())
				}
			} else {
				reason = "CreateFailedRetrying"
				if errors.Is(tc.cause, providers.ErrTerminal) {
					reason = "CreateFailed"
				}
				counter = metrics.MachineFailureTotal.WithLabelValues("Create", providers.ClassifyOutcome(tc.cause))
			}
			before := testutil.ToFloat64(counter)
			if err := record(); !errors.Is(err, patchErr) {
				t.Fatalf("record() error = %v, want status patch error", err)
			}
			if got := testutil.ToFloat64(counter); got != before {
				t.Fatalf("failed patch changed counter from %v to %v", before, got)
			}
			// Re-read persisted state as a subsequent reconciliation would.
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(machine), machine); err != nil {
				t.Fatal(err)
			}
			if len(machine.Status.Conditions) != 0 {
				t.Fatalf("failed patch persisted conditions: %v", machine.Status.Conditions)
			}
			failPatch = false
			if err := record(); err != nil {
				t.Fatal(err)
			}
			if got := testutil.ToFloat64(counter); got != before+1 {
				t.Fatalf("successful patch changed counter from %v to %v, want one observation", before, got)
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(machine), machine); err != nil {
				t.Fatal(err)
			}
			ready := apimeta.FindStatusCondition(machine.Status.Conditions, infrav1.MachineReadyCondition)
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != reason {
				t.Fatalf("persisted Ready condition = %v, want False with reason %s", ready, reason)
			}
			failed := apimeta.IsStatusConditionTrue(machine.Status.Conditions, infrav1.MachineProvisioningFailedCondition)
			if failed != errors.Is(tc.cause, providers.ErrTerminal) {
				t.Fatalf("ProvisioningFailed = %v, inconsistent with terminality", failed)
			}
		})
	}
}
