/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that callWithRetryAndVerification() can be stopped from retrying by returning a permanent error
func TestCallWithRetryAndVerificationPermanentError(t *testing.T) {
	t.Parallel()

	const timeout = 10 * time.Second
	ctx, cancel := testutil.GetTestContext(t, timeout)
	defer cancel()

	attempt := 0
	start := time.Now()
	permanentError := errors.New("permanent error")
	temporaryError := errors.New("temporary error")

	_, err := callWithRetryAndVerification(
		ctx,
		exponentialBackoff(timeout),
		func(_ context.Context) error { return nil },
		func(_ context.Context) (string, error) {
			attempt++
			if attempt == 2 {
				return "", resiliency.Permanent(permanentError)
			}
			return "", temporaryError
		},
	)

	require.Error(t, err)
	require.ErrorIs(t, err, permanentError)
	require.Equal(t, 2, attempt)
	require.WithinRangef(t, time.Now(), start, start.Add(timeout/2), "the call should have stopped after the second attmept, much sooner than the timeout")
}

func TestSaveChangesAcknowledgesStatusAfterSuccessfulSave(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-service"}}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv1.Service{}).
		WithObjects(service).
		Build()
	currentService := &apiv1.Service{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: service.Name}, currentService))
	patch := ctrl_client.MergeFromWithOptions(currentService.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	currentService.Status.State = apiv1.ServiceStateReady
	reconciler := NewReconcilerBase[apiv1.Service](client, client, logr.Discard(), context.Background())

	acknowledged := false
	_, saveErr := reconciler.SaveChanges(context.Background(), currentService, patch, statusChanged, func() {
		persistedService := &apiv1.Service{}
		require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: service.Name}, persistedService))
		require.Equal(t, apiv1.ServiceStateReady, persistedService.Status.State)
		acknowledged = true
	}, logr.Discard())

	require.NoError(t, saveErr)
	require.True(t, acknowledged)
}

func TestSaveChangesAcknowledgesStatusWhenAlreadyDurable(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-service"}}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	patch := ctrl_client.MergeFromWithOptions(service.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	reconciler := NewReconcilerBase[apiv1.Service](client, client, logr.Discard(), context.Background())

	acknowledged := false
	_, saveErr := reconciler.SaveChanges(context.Background(), service, patch, additionalReconciliationNeeded, func() {
		acknowledged = true
	}, logr.Discard())

	require.NoError(t, saveErr)
	require.True(t, acknowledged)
}

func TestSaveChangesDoesNotAcknowledgeStatusAfterConflict(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv1.AddToScheme(scheme))
	service := &apiv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-service"}}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&apiv1.Service{}).
		WithObjects(service).
		Build()
	currentService := &apiv1.Service{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Name: service.Name}, currentService))
	patch := ctrl_client.MergeFromWithOptions(currentService.DeepCopy(), ctrl_client.MergeFromWithOptimisticLock{})
	concurrentService := currentService.DeepCopy()
	concurrentService.Labels = map[string]string{"updated": "true"}
	require.NoError(t, client.Update(context.Background(), concurrentService))
	currentService.Status.State = apiv1.ServiceStateReady
	reconciler := NewReconcilerBase[apiv1.Service](client, client, logr.Discard(), context.Background())

	acknowledged := false
	result, saveErr := reconciler.SaveChanges(context.Background(), currentService, patch, statusChanged, func() {
		acknowledged = true
	}, logr.Discard())

	require.NoError(t, saveErr)
	require.Positive(t, result.RequeueAfter)
	require.False(t, acknowledged)
}
