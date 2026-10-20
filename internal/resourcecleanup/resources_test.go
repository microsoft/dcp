/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resourcecleanup

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
)

func TestShutdownResourcesIncludeV1AndV2Namespace(t *testing.T) {
	shutdownResourceGVRs := cleanupResourceGVRSet(ShutdownResources)

	require.Contains(t, shutdownResourceGVRs, (&apiv1.Container{}).GetGroupVersionResource())
	require.Contains(t, shutdownResourceGVRs, (&apiv1.ContainerNetwork{}).GetGroupVersionResource())
	require.Contains(t, shutdownResourceGVRs, (&apiv2.Namespace{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.LogicalContainer{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.LogicalContainerNetwork{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.LogicalProcess{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.PhysicalContainer{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.PhysicalContainerImage{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.PhysicalContainerNetwork{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.PhysicalContainerVolume{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.PhysicalProcess{}).GetGroupVersionResource())
}

// Verifies that NamespaceResources covers every registered namespace-scoped V2 kind.
func TestNamespaceResourcesIncludeAllNamespacedV2Kinds(t *testing.T) {
	namespaceResourceGVRs := cleanupResourceGVRSet(NamespaceResources)
	for _, resource := range apiv2.PersistentTypes {
		if resource.NamespaceScoped() {
			require.Contains(t, namespaceResourceGVRs, resource.GetGroupVersionResource())
		}
	}
}

// Verifies that NamespaceResources cleans logical processes before physical processes,
// containers before logical networks, and logical networks before physical networks.
func TestNamespaceResourcesCleanLogicalResourcesBeforePhysicalDependencies(t *testing.T) {
	namespaceResourcesByGVR := cleanupResourcesByGVR(NamespaceResources)
	containerGVR := (&apiv2.LogicalContainer{}).GetGroupVersionResource()
	logicalNetworkGVR := (&apiv2.LogicalContainerNetwork{}).GetGroupVersionResource()
	physicalContainerGVR := (&apiv2.PhysicalContainer{}).GetGroupVersionResource()
	physicalContainerImageGVR := (&apiv2.PhysicalContainerImage{}).GetGroupVersionResource()
	physicalContainerNetworkGVR := (&apiv2.PhysicalContainerNetwork{}).GetGroupVersionResource()
	physicalContainerNetworkConnectionGVR := (&apiv2.PhysicalContainerNetworkConnection{}).GetGroupVersionResource()
	physicalContainerVolumeGVR := (&apiv2.PhysicalContainerVolume{}).GetGroupVersionResource()
	physicalProcessGVR := (&apiv2.PhysicalProcess{}).GetGroupVersionResource()
	logicalProcessGVR := (&apiv2.LogicalProcess{}).GetGroupVersionResource()

	require.Len(t, namespaceResourcesByGVR, 11)
	require.Contains(t, namespaceResourcesByGVR, containerGVR)
	require.Contains(t, namespaceResourcesByGVR, logicalNetworkGVR)
	require.Contains(t, namespaceResourcesByGVR, physicalContainerGVR)
	require.Contains(t, namespaceResourcesByGVR, physicalContainerImageGVR)
	require.Contains(t, namespaceResourcesByGVR, physicalContainerNetworkGVR)
	require.Contains(t, namespaceResourcesByGVR, physicalContainerNetworkConnectionGVR)
	require.Contains(t, namespaceResourcesByGVR, physicalContainerVolumeGVR)
	require.Contains(t, namespaceResourcesByGVR, physicalProcessGVR)
	require.Contains(t, namespaceResourcesByGVR[physicalProcessGVR].CleanUpAfter, logicalProcessGVR)
	require.Empty(t, namespaceResourcesByGVR[logicalProcessGVR].CleanUpAfter)
	logicalExecGVR := (&apiv2.LogicalContainerExec{}).GetGroupVersionResource()
	physicalExecGVR := (&apiv2.PhysicalContainerExec{}).GetGroupVersionResource()
	require.Empty(t, namespaceResourcesByGVR[logicalExecGVR].CleanUpAfter)
	require.Contains(t, namespaceResourcesByGVR[containerGVR].CleanUpAfter, logicalExecGVR)
	require.Contains(t, namespaceResourcesByGVR[containerGVR].CleanUpAfter, physicalExecGVR)
	require.Contains(t, namespaceResourcesByGVR[physicalExecGVR].CleanUpAfter, logicalExecGVR)
	require.Contains(t, namespaceResourcesByGVR[physicalContainerGVR].CleanUpAfter, physicalExecGVR)
	require.Empty(t, namespaceResourcesByGVR[physicalContainerNetworkConnectionGVR].CleanUpAfter)
	require.Contains(t, namespaceResourcesByGVR[physicalContainerGVR].CleanUpAfter, physicalContainerNetworkConnectionGVR)
	require.Contains(t, namespaceResourcesByGVR[physicalContainerGVR].CleanUpAfter, containerGVR)
	require.Contains(t, namespaceResourcesByGVR[physicalContainerImageGVR].CleanUpAfter, physicalContainerGVR)
	require.Contains(t, namespaceResourcesByGVR[logicalNetworkGVR].CleanUpAfter, physicalContainerGVR)
	require.Contains(t, namespaceResourcesByGVR[physicalContainerNetworkGVR].CleanUpAfter, logicalNetworkGVR)
	// A network cannot be removed while containers are still attached to it.
	require.Contains(t, namespaceResourcesByGVR[physicalContainerNetworkGVR].CleanUpAfter, physicalContainerGVR)
	// A volume cannot be removed while a container still references it.
	require.Contains(t, namespaceResourcesByGVR[physicalContainerVolumeGVR].CleanUpAfter, physicalContainerGVR)
}

func cleanupResourceGVRSet(resources []*CleanupResource) map[schema.GroupVersionResource]struct{} {
	resourceGVRs := map[schema.GroupVersionResource]struct{}{}
	for _, resource := range resources {
		resourceGVRs[resource.GVR] = struct{}{}
	}

	return resourceGVRs
}

func cleanupResourcesByGVR(resources []*CleanupResource) map[schema.GroupVersionResource]*CleanupResource {
	resourcesByGVR := map[schema.GroupVersionResource]*CleanupResource{}
	for _, resource := range resources {
		resourcesByGVR[resource.GVR] = resource
	}

	return resourcesByGVR
}
