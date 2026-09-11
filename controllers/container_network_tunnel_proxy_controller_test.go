/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	apiv2 "github.com/microsoft/dcp/api/v2"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func TestRemoveUnusedTunnelProxyBuildContext(t *testing.T) {
	source := createTunnelProxyBuildContextTestFile(t)

	removeUnusedTunnelProxyBuildContext(source, nil, logr.Discard())

	require.NoFileExists(t, source)
}

func TestRemoveUnusedTunnelProxyBuildContextPreservesReferencedSource(t *testing.T) {
	source := createTunnelProxyBuildContextTestFile(t)
	physicalImage := &apiv2.PhysicalContainerImage{
		Spec: apiv2.PhysicalContainerImageSpec{
			Image: &apiv2.PhysicalContainerImageConfig{
				Build: &apiv2.ContainerBuildContext{
					ContextArchive: &apiv2.ContainerBuildContextArchive{
						Source: source,
					},
				},
			},
		},
	}

	removeUnusedTunnelProxyBuildContext(source, physicalImage, logr.Discard())

	require.FileExists(t, source)
}

func createTunnelProxyBuildContextTestFile(t *testing.T) string {
	t.Helper()

	source := filepath.Join(t.TempDir(), "build-context.tar")
	file, openErr := usvc_io.CreateNewFile(source, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, openErr)
	require.NoError(t, file.Close())
	return source
}
