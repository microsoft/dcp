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
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.PhysicalContainer{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.PhysicalContainerImage{}).GetGroupVersionResource())
	require.NotContains(t, shutdownResourceGVRs, (&apiv2.PhysicalContainerNetwork{}).GetGroupVersionResource())
}

func TestNamespaceResourcesCleanPhysicalContainersFirst(t *testing.T) {
	namespaceResourcesByGVR := cleanupResourcesByGVR(NamespaceResources)
	physicalContainerGVR := (&apiv2.PhysicalContainer{}).GetGroupVersionResource()
	physicalContainerImageGVR := (&apiv2.PhysicalContainerImage{}).GetGroupVersionResource()
	physicalContainerNetworkGVR := (&apiv2.PhysicalContainerNetwork{}).GetGroupVersionResource()

	require.Len(t, namespaceResourcesByGVR, 3)
	require.Contains(t, namespaceResourcesByGVR, physicalContainerGVR)
	require.Contains(t, namespaceResourcesByGVR, physicalContainerImageGVR)
	require.Contains(t, namespaceResourcesByGVR, physicalContainerNetworkGVR)
	require.Contains(t, namespaceResourcesByGVR[physicalContainerImageGVR].CleanUpAfter, physicalContainerGVR)
	// A network cannot be removed while containers are still attached to it.
	require.Contains(t, namespaceResourcesByGVR[physicalContainerNetworkGVR].CleanUpAfter, physicalContainerGVR)
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
