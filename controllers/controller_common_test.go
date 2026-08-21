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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv2 "github.com/microsoft/dcp/api/v2"
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

func TestAfterStatusUpdateIsDurable(t *testing.T) {
	t.Parallel()

	t.Run("waits for successful status save", func(t *testing.T) {
		t.Parallel()

		acknowledged := false
		onSuccessfulSave := afterStatusUpdateIsDurable(statusChanged, func() {
			acknowledged = true
		})

		require.False(t, acknowledged)
		require.NotNil(t, onSuccessfulSave)
		onSuccessfulSave()
		require.True(t, acknowledged)
	})

	t.Run("acknowledges status that is already durable", func(t *testing.T) {
		t.Parallel()

		acknowledged := false
		onSuccessfulSave := afterStatusUpdateIsDurable(noChange, func() {
			acknowledged = true
		})

		require.True(t, acknowledged)
		require.Nil(t, onSuccessfulSave)
	})
}

func TestEnsureNamespaceRequiresFinalizedActiveNamespace(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))

	ctx := context.Background()
	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-namespace",
		},
		Status: apiv2.NamespaceStatus{
			Phase: apiv2.NamespacePhaseActive,
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build()

	var pendingMessage string
	ready, change := ensureNamespace(ctx, client, namespace.Name, func(message string) objectChange {
		pendingMessage = message
		return statusChanged
	}, func(string) objectChange {
		t.Fatal("unexpected failed status")
		return noChange
	}, logr.Discard())

	require.False(t, ready)
	require.Equal(t, statusChanged, change)
	require.Equal(t, `Namespace "test-namespace" is not ready.`, pendingMessage)

	namespace.Finalizers = []string{namespaceFinalizer}
	require.NoError(t, client.Update(ctx, namespace))

	ready, change = ensureNamespace(ctx, client, namespace.Name, func(string) objectChange {
		t.Fatal("unexpected pending status")
		return noChange
	}, func(string) objectChange {
		t.Fatal("unexpected failed status")
		return noChange
	}, logr.Discard())

	require.True(t, ready)
	require.Equal(t, noChange, change)
}
