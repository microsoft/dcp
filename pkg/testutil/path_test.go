/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that CreateTestSessionDir creates an owner-restricted directory under the selected
// test root, including a protected Windows DACL suitable for an explicit state-store path.
func TestCreateTestSessionDirIsOwnerRestricted(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_TEMPDIRECTORY", root)
	sessionDir, createErr := testutil.CreateTestSessionDir()
	require.NoError(t, createErr)
	require.Equal(t, root, filepath.Dir(sessionDir))
	require.NoError(t, usvc_io.ValidateRestrictedDirectory(sessionDir, osutil.PermissionOnlyOwnerReadWriteTraverse))
}
