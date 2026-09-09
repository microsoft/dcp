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
	containerStarted := make(chan struct{})
	processGVR := (&apiv2.PhysicalProcess{}).GetGroupVersionResource()
	containerGVR := (&apiv2.PhysicalContainer{}).GetGroupVersionResource()
	namespaceCleanupResourceHandlers[processGVR] = func(
		_ *NamespaceReconciler,
		ctx context.Context,
		_ *apiv2.Namespace,
		_ logr.Logger,
	) (int, error) {
		close(processStarted)
		select {
		case <-containerStarted:
			return 1, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	namespaceCleanupResourceHandlers[containerGVR] = func(
		_ *NamespaceReconciler,
		ctx context.Context,
		_ *apiv2.Namespace,
		_ logr.Logger,
	) (int, error) {
		close(containerStarted)
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
	require.Equal(t, "1 physicalprocesses and 1 physicalcontainers", pending)
}
