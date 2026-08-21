/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/resourcecleanup"
)

func TestNamespaceCleanupResourcesHaveHandlers(t *testing.T) {
	t.Parallel()

	namespaceResourceGVRs := map[schema.GroupVersionResource]struct{}{}
	for _, namespaceResource := range resourcecleanup.NamespaceResources {
		namespaceResourceGVRs[namespaceResource.GVR] = struct{}{}
		require.Contains(t, namespaceCleanupResourceHandlers, namespaceResource.GVR)
	}

	require.Len(t, namespaceCleanupResourceHandlers, len(namespaceResourceGVRs))
}

func TestManageNamespaceKeepsMonitoringActiveNamespace(t *testing.T) {
	t.Parallel()

	reconciler := &NamespaceReconciler{}
	namespace := &apiv2.Namespace{}

	require.Equal(t, statusChanged|additionalReconciliationNeeded, reconciler.manageNamespace(namespace))
	require.Equal(t, apiv2.NamespacePhaseActive, namespace.Status.Phase)
	require.Equal(t, MonitoringDelay, namespaceReconciliationDelay(namespace))

	require.Equal(t, additionalReconciliationNeeded, reconciler.manageNamespace(namespace))
}

func TestTerminatingNamespaceUsesStandardReconciliationDelay(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	namespace := &apiv2.Namespace{
		ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
		Status:     apiv2.NamespaceStatus{Phase: apiv2.NamespacePhaseActive},
	}

	require.Equal(t, StandardDelay, namespaceReconciliationDelay(namespace))
}

func TestSetNamespaceCleanupInProgressNamesPendingResources(t *testing.T) {
	t.Parallel()

	reconciler := &NamespaceReconciler{}
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-namespace", Generation: 1}}

	require.Equal(t, statusChanged, reconciler.setNamespaceCleanupInProgress(namespace, ""))
	condition := apimeta.FindStatusCondition(namespace.Status.Conditions, namespaceCleanupCompleteCondition)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, namespaceCleanupInProgressReason, condition.Reason)
	require.Equal(t, "Namespace cleanup is in progress.", condition.Message)

	require.Equal(t, statusChanged, reconciler.setNamespaceCleanupInProgress(namespace, "2 physicalcontainers"))
	condition = apimeta.FindStatusCondition(namespace.Status.Conditions, namespaceCleanupCompleteCondition)
	require.Equal(t, "Namespace cleanup is waiting for 2 physicalcontainers to be deleted.", condition.Message)

	// A stalled cleanup reports the same message every reconciliation and must not keep writing status.
	require.Equal(t, noChange, reconciler.setNamespaceCleanupInProgress(namespace, "2 physicalcontainers"))
}

func TestNamespaceCleanupFailureIsRecordedAndCleared(t *testing.T) {
	t.Parallel()

	reconciler := &NamespaceReconciler{}
	namespace := &apiv2.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test-namespace", Generation: 1}}

	failureChange := setCondition(
		&namespace.Status.Conditions,
		namespaceCleanupCompleteCondition,
		namespace.Generation,
		metav1.ConditionFalse,
		namespaceCleanupFailedReason,
		"Namespace cleanup failed: boom",
	)
	require.Equal(t, statusChanged, failureChange)

	condition := apimeta.FindStatusCondition(namespace.Status.Conditions, namespaceCleanupCompleteCondition)
	require.Equal(t, namespaceCleanupFailedReason, condition.Reason)
	require.Contains(t, condition.Message, "boom")

	// Progress after a failure must replace the recorded failure rather than leaving it stale.
	require.Equal(t, statusChanged, reconciler.setNamespaceCleanupInProgress(namespace, "1 physicalcontainerimages"))
	condition = apimeta.FindStatusCondition(namespace.Status.Conditions, namespaceCleanupCompleteCondition)
	require.Equal(t, namespaceCleanupInProgressReason, condition.Reason)
	require.NotContains(t, condition.Message, "boom")
}
