//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
)

func requireTunnelProxyCredentialPermissions(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, usvc_io.ValidateRestrictedDirectory(filepath.Dir(path), osutil.PermissionOnlyOwnerReadWriteTraverse))
	fileInfo, statErr := os.Stat(path)
	require.NoError(t, statErr)
	require.Equal(t, osutil.PermissionOnlyOwnerReadWrite, fileInfo.Mode().Perm())
}
