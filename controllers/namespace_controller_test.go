/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/pkg/testutil"
)

type concurrentCleanupLists struct {
	ctrl_client.Client
	mapper            apimeta.RESTMapper
	processStarted    chan struct{}
	connectionStarted chan struct{}
}

func (client *concurrentCleanupLists) RESTMapper() apimeta.RESTMapper { return client.mapper }

func (client *concurrentCleanupLists) List(ctx context.Context, list ctrl_client.ObjectList, _ ...ctrl_client.ListOption) error {
	var waitFor <-chan struct{}
	switch list.GetObjectKind().GroupVersionKind().Kind {
	case "LogicalProcessList":
		close(client.processStarted)
		waitFor = client.connectionStarted
	case "PhysicalContainerNetworkConnectionList":
		close(client.connectionStarted)
		waitFor = client.processStarted
	case "PhysicalContainerList":
		return fmt.Errorf("dependent container cleanup started before connection cleanup completed")
	default:
		return nil
	}
	select {
	case <-waitFor:
	case <-ctx.Done():
		return ctx.Err()
	}
	pending := unstructured.Unstructured{}
	deleting := metav1.Now()
	pending.SetDeletionTimestamp(&deleting)
	list.(*unstructured.UnstructuredList).Items = []unstructured.Unstructured{pending}
	return nil
}

// Verifies that metadata-driven cleanup lists independent resource kinds concurrently
// while pending objects prevent subsequent dependency batches from starting.
func TestNamespaceCleanupProcessesReadyResourceKindsConcurrently(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv2.AddToScheme(scheme))
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{apiv2.GroupVersion})
	for _, resource := range apiv2.PersistentTypes {
		kinds, _, kindsErr := scheme.ObjectKinds(resource)
		require.NoError(t, kindsErr)
		require.Len(t, kinds, 1)
		mapper.Add(kinds[0], apimeta.RESTScopeNamespace)
	}
	lists := &concurrentCleanupLists{
		mapper: mapper, processStarted: make(chan struct{}), connectionStarted: make(chan struct{}),
	}
	reconciler := &NamespaceReconciler{
		ReconcilerBase: &ReconcilerBase[apiv2.Namespace, *apiv2.Namespace]{Client: lists, NoCacheClient: lists},
	}
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
	pending, cleanupErr := reconciler.cleanupNamespace(ctx, namespace, logr.Discard())
	require.NoError(t, cleanupErr)
	require.Equal(t, "1 logicalprocesses and 1 physicalcontainernetworkconnections", pending)
}
