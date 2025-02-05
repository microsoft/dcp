// Copyright (c) Microsoft Corporation. All rights reserved.

// Package v1 contains API Schema definitions for the usvc-dev v1 API group
// +kubebuilder:object:generate=true
// +groupName=usvc-dev.developer.microsoft.com
package v1

import (
	"errors"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"

	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

// +k8s:deepcopy-gen=false
type CleanupResource struct {
	GVR schema.GroupVersionResource

	// Specifies what other resource kinds need to be cleaned up first before the given resource can be cleaned up.
	CleanUpAfter []schema.GroupVersionResource
}

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: "usvc-dev.developer.microsoft.com", Version: "v1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme

	// Track the resources that need to be automatically cleaned up at shutdown
	CleanupResources = []*CleanupResource{
		{
			GVR: (&ContainerExec{}).GetGroupVersionResource(),
		},
		{
			GVR: (&ExecutableReplicaSet{}).GetGroupVersionResource(),
		},
		{
			GVR: (&Service{}).GetGroupVersionResource(),
		},
		{
			GVR: (&Container{}).GetGroupVersionResource(),
			CleanUpAfter: []schema.GroupVersionResource{
				(&ContainerExec{}).GetGroupVersionResource(),
			},
		},
		{
			GVR:          (&Executable{}).GetGroupVersionResource(),
			CleanUpAfter: []schema.GroupVersionResource{(&ExecutableReplicaSet{}).GetGroupVersionResource()},
		},
		{
			GVR: (&ContainerNetworkConnection{}).GetGroupVersionResource(),
			CleanUpAfter: []schema.GroupVersionResource{
				(&Container{}).GetGroupVersionResource(), // Remove orphaned ContainerNetworkConnections after all Containers are deleted
			},
		},
		{
			GVR: (&ContainerNetwork{}).GetGroupVersionResource(),
			CleanUpAfter: []schema.GroupVersionResource{
				(&Container{}).GetGroupVersionResource(),
				(&ContainerNetworkConnection{}).GetGroupVersionResource(),
			},
		},
	}

	// A registry of resource log streaming implementations
	ResourceLogStreamers = &syncmap.Map[schema.GroupVersionResource, ResourceLogStreamer]{}

	// Whether new resource creation is prohibited (because the API server is shutting down)
	ResourceCreationProhibited    = &atomic.Bool{}
	errResourceCreationProhibited = errors.New("new resources cannot be created because the API server is shutting down")
)
