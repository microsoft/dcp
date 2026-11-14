/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
)

// Verifies that instance-local timing controls both delayed event delivery and requeue results,
// agrees with retry eligibility, and leaves other reconcilers' production timing unchanged.
func TestReconciliationTimingIsInstanceLocal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(test *testing.T) {
		ctx, cancel := context.WithCancel(test.Context())
		defer cancel()
		timing := ReconciliationTiming{
			StandardDelay: 100 * time.Millisecond, RetryDelay: 200 * time.Millisecond,
			MonitoringDelay: time.Second, DebounceDelay: 20 * time.Millisecond, MaxDebounceDelay: 100 * time.Millisecond,
		}
		configured := NewReconcilerBase[apiv2.PhysicalContainerNetwork](nil, nil, logr.Discard(), ctx)
		production := NewReconcilerBase[apiv2.PhysicalContainerNetwork](nil, nil, logr.Discard(), ctx)
		require.NoError(test, configured.ConfigureReconciliationTiming(timing))
		timing.RetryDelay = time.Minute
		resource := &apiv2.PhysicalContainerNetwork{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "test"}}
		queued, queueErr := configured.SaveChangesWithDelay(ctx, resource, nil, additionalReconciliationNeeded, LongDelay, nil, logr.Discard())
		require.NoError(test, queueErr)
		require.Equal(test, 200*time.Millisecond, queued.RequeueAfter)
		unchanged, unchangedErr := production.SaveChangesWithDelay(ctx, resource, nil, additionalReconciliationNeeded, LongDelay, nil, logr.Discard())
		require.NoError(test, unchangedErr)
		require.GreaterOrEqual(test, unchanged.RequeueAfter, 5*time.Second)
		require.Less(test, unchanged.RequeueAfter, 7*time.Second)
		monitoring, monitoringErr := configured.SaveChangesWithDelay(ctx, resource, nil, additionalReconciliationNeeded, MonitoringDelay, nil, logr.Discard())
		require.NoError(test, monitoringErr)
		require.Equal(test, time.Second, monitoring.RequeueAfter)

		started := time.Now()
		retryAfter := started.Add(configured.ReconciliationDelay(LongDelay))
		configured.ScheduleReconciliationWithDelay(types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, LongDelay)
		select {
		case event, open := <-configured.notifyRunChanged.Out:
			require.True(test, open)
			require.Equal(test, resource.Name, event.Object.GetName())
			require.Equal(test, resource.Namespace, event.Object.GetNamespace())
			require.Equal(test, 220*time.Millisecond, time.Since(started))
			require.False(test, time.Now().Before(retryAfter), "scheduled retry must already be eligible")
		case <-ctx.Done():
			test.Fatal("configured reconciliation was not delivered")
		}
	})
}

// Verifies that invalid timing configuration leaves the prior scheduling policy in effect.
func TestReconciliationTimingRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, invalid := range []ReconciliationTiming{
		{},
		{StandardDelay: time.Second, RetryDelay: -time.Second, MonitoringDelay: time.Second, DebounceDelay: time.Second, MaxDebounceDelay: time.Second},
		{StandardDelay: time.Second, RetryDelay: time.Second, MonitoringDelay: time.Second, DebounceDelay: time.Second, MaxDebounceDelay: time.Millisecond},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		reconciler := NewReconcilerBase[apiv2.PhysicalContainerVolume](nil, nil, logr.Discard(), ctx)
		require.Error(t, reconciler.ConfigureReconciliationTiming(invalid))
		resource := &apiv2.PhysicalContainerVolume{}
		result, resultErr := reconciler.SaveChangesWithDelay(ctx, resource, nil, additionalReconciliationNeeded, LongDelay, nil, logr.Discard())
		require.NoError(t, resultErr)
		require.GreaterOrEqual(t, result.RequeueAfter, 5*time.Second)
		require.Less(t, result.RequeueAfter, 7*time.Second)
		cancel()
	}
}
