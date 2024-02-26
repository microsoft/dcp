// Copyright (c) Microsoft Corporation. All rights reserved.

// Package v1 contains API Schema definitions for the usvc-dev v1 API group
// +kubebuilder:object:generate=true
// +groupName=usvc-dev.developer.microsoft.com
package v1

import (
	"sync"

	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"golang.org/x/exp/slices"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"

	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

// +k8s:deepcopy-gen=false
type WeightedResource struct {
	Object resource.Object
	Weight uint
}

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: "usvc-dev.developer.microsoft.com", Version: "v1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme

	// Track the resources that need to be automatically cleaned up at shutdown
	// Uses weighting to cleanup resources in batches
	CleanupResources = []*WeightedResource{}

	// A registry of log stream factories for different resource types
	LogStreamFactories = syncmap.Map[schema.GroupVersionResource, ResourceLogStreamFactory]{}
)

var (
	resourceMutex = sync.Mutex{}
)

func SetCleanupPriority(obj resource.Object, weight uint) {
	weightedResource := &WeightedResource{
		Object: obj,
		Weight: weight,
	}

	resourceMutex.Lock()
	defer resourceMutex.Unlock()

	index, _ := slices.BinarySearchFunc(CleanupResources, weightedResource, func(a *WeightedResource, b *WeightedResource) int {
		if a.Weight < b.Weight {
			return -1
		} else if a.Weight > b.Weight {
			return 1
		} else {
			return 0
		}
	})

	CleanupResources = slices.Insert(CleanupResources, index, weightedResource)
}
