/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetTestContainerToolPathPreservesExactFilename(t *testing.T) {
	t.Parallel()

	const toolName = "container_probe_c"
	toolPath, toolPathErr := GetTestContainerToolPath(toolName)
	require.NoError(t, toolPathErr)
	require.Equal(t, toolName, filepath.Base(toolPath))
}
