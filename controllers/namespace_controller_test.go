/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
