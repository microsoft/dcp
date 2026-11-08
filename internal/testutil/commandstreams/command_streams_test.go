/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commandstreams

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that command-stream test helpers and their mocking dependencies are absent
// from the production DCP dependency graph.
func TestProductionDependenciesExcludeTestHelpers(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	root, rootErr := osutil.FindRootFor(osutil.FileTarget, "go.mod")
	require.NoError(t, rootErr)
	command := exec.CommandContext(ctx, "go", "list", "-deps", "./cmd/dcp")
	command.Dir = root
	output, listErr := command.CombinedOutput()
	require.NoError(t, listErr, "%s", output)
	dependencies := strings.Fields(string(output))
	for _, testDependency := range []string{
		"github.com/microsoft/dcp/internal/testutil/commandstreams",
		"github.com/microsoft/dcp/pkg/testutil",
		"github.com/stretchr/testify/mock",
		"github.com/stretchr/objx",
	} {
		require.NotContains(t, dependencies, testDependency)
	}
}
