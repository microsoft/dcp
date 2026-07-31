/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2

import (
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const (
	// GroupName is the group name used for DCP V2 APIs.
	GroupName = "usvc-dev.developer.microsoft.com"

	// Version is the version used for DCP V2 APIs.
	Version = "v2"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme

	// Types that have data stored in the API server.
	PersistentTypes = []apiserver_resource.Object{
		&Namespace{},
		&PhysicalContainerImage{},
		&PhysicalContainer{},
		&PhysicalContainerNetwork{},
	}

	// Types that must be recognizable by the API server, but are not persisted.
	AdditionalTypes = []apiserver_resource.Object{}
)
