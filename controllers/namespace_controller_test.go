/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that namespace cleanup runs logical processes and physical connections concurrently
// while their dependent resource kinds remain blocked.
func TestNamespaceCleanupProcessesReadyResourceKindsConcurrently(t *testing.T) {
	const timeout = 10 * time.Second
	ctx, cancel := testutil.GetTestContext(t, timeout)
	defer cancel()

	originalHandlers := namespaceCleanupResourceHandlers
	namespaceCleanupResourceHandlers = make(map[schema.GroupVersionResource]namespaceCleanupResourceHandler, len(originalHandlers))
	for gvr, handler := range originalHandlers {
		namespaceCleanupResourceHandlers[gvr] = handler
	}
	t.Cleanup(func() {
		namespaceCleanupResourceHandlers = originalHandlers
	})

	processStarted := make(chan struct{})
	connectionStarted := make(chan struct{})
	processGVR := (&apiv2.LogicalProcess{}).GetGroupVersionResource()
	connectionGVR := (&apiv2.PhysicalContainerNetworkConnection{}).GetGroupVersionResource()
	namespaceCleanupResourceHandlers[(&apiv2.LogicalContainer{}).GetGroupVersionResource()] = func(
		_ *NamespaceReconciler,
		_ context.Context,
		_ *apiv2.Namespace,
		_ logr.Logger,
	) (int, error) {
		return 0, nil
	}
	namespaceCleanupResourceHandlers[processGVR] = func(
		_ *NamespaceReconciler,
		ctx context.Context,
		_ *apiv2.Namespace,
		_ logr.Logger,
	) (int, error) {
		close(processStarted)
		select {
		case <-connectionStarted:
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	namespaceCleanupResourceHandlers[connectionGVR] = func(
		_ *NamespaceReconciler,
		ctx context.Context,
		_ *apiv2.Namespace,
		_ logr.Logger,
	) (int, error) {
		close(connectionStarted)
		select {
		case <-processStarted:
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	reconciler := &NamespaceReconciler{}
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
	pending, cleanupErr := reconciler.cleanupNamespace(ctx, namespace, logr.Discard())

	require.NoError(t, cleanupErr)
	require.Equal(t, "1 logicalprocesses and 1 physicalcontainernetworkconnections", pending)
}
