/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"

	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/syncmap"
)

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: "usvc-dev.developer.microsoft.com", Version: "v1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme

	// A registry of resource log streaming implementations
	ResourceLogStreamers = &syncmap.Map[schema.GroupVersionResource, ResourceLogStreamer]{}

	// Whether new resource creation is prohibited (because the API server is shutting down)
	ResourceCreationProhibited    = commonapi.ResourceCreationProhibited
	errResourceCreationProhibited = commonapi.ErrResourceCreationProhibited

	// Types that have data stored in the API server.
	PersistentTypes = []apiserver_resource.Object{
		&Executable{},
		&Endpoint{},
		&ExecutableReplicaSet{},
		&Container{},
		&ContainerVolume{},
		&ContainerNetwork{},
		&ContainerNetworkConnection{},
		&ContainerExec{},
		&Service{},
		&ContainerNetworkTunnelProxy{},
	}

	// Types that must be recognizable by the API server, but are not persisted
	// (they are used for request processing only).
	AddtionalTypes = []apiserver_resource.Object{
		&LogOptions{},
		&LogStreamer{},
	}
)
