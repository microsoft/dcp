/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resourcecleanup

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	apiv1 "github.com/microsoft/dcp/api/v1"
	apiv2 "github.com/microsoft/dcp/api/v2"
)

// CleanupResource describes a resource kind and the resource kinds that must be cleaned up first.
type CleanupResource struct {
	GVR schema.GroupVersionResource

	// CleanUpAfter specifies what other resource kinds need to be cleaned up first.
	CleanUpAfter []schema.GroupVersionResource
}

// ShutdownResources are resource kinds that need to be automatically cleaned up at shutdown.
var ShutdownResources = []*CleanupResource{
	{
		GVR: (&apiv1.ContainerExec{}).GetGroupVersionResource(),
	},
	{
		GVR: (&apiv1.ExecutableReplicaSet{}).GetGroupVersionResource(),
	},
	{
		GVR: (&apiv1.Service{}).GetGroupVersionResource(),
	},
	{
		GVR: (&apiv1.Container{}).GetGroupVersionResource(),
		CleanUpAfter: []schema.GroupVersionResource{
			(&apiv1.ContainerExec{}).GetGroupVersionResource(),
		},
	},
	{
		GVR:          (&apiv1.Executable{}).GetGroupVersionResource(),
		CleanUpAfter: []schema.GroupVersionResource{(&apiv1.ExecutableReplicaSet{}).GetGroupVersionResource()},
	},
	{
		GVR: (&apiv1.ContainerNetworkConnection{}).GetGroupVersionResource(),
		CleanUpAfter: []schema.GroupVersionResource{
			(&apiv1.Container{}).GetGroupVersionResource(),
		},
	},
	{
		GVR: (&apiv1.ContainerNetwork{}).GetGroupVersionResource(),
		CleanUpAfter: []schema.GroupVersionResource{
			(&apiv1.Container{}).GetGroupVersionResource(),
			(&apiv1.ContainerNetworkConnection{}).GetGroupVersionResource(),
			(&apiv1.ContainerNetworkTunnelProxy{}).GetGroupVersionResource(),
		},
	},
	{
		GVR: (&apiv1.ContainerVolume{}).GetGroupVersionResource(),
		CleanUpAfter: []schema.GroupVersionResource{
			(&apiv1.Container{}).GetGroupVersionResource(),
		},
	},
	{
		GVR: (&apiv1.ContainerNetworkTunnelProxy{}).GetGroupVersionResource(),
	},
	{
		GVR: (&apiv2.Namespace{}).GetGroupVersionResource(),
	},
}

// NamespaceResources are namespace-scoped V2 resource kinds that are deleted when a V2 Namespace is deleted.
var NamespaceResources = []*CleanupResource{
	{
		GVR: (&apiv2.PhysicalContainer{}).GetGroupVersionResource(),
	},
	{
		GVR: (&apiv2.PhysicalContainerImage{}).GetGroupVersionResource(),
		CleanUpAfter: []schema.GroupVersionResource{
			(&apiv2.PhysicalContainer{}).GetGroupVersionResource(),
		},
	},
	{
		GVR: (&apiv2.PhysicalNetwork{}).GetGroupVersionResource(),
		CleanUpAfter: []schema.GroupVersionResource{
			(&apiv2.PhysicalContainer{}).GetGroupVersionResource(),
		},
	},
}
