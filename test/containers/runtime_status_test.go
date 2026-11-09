/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containers_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/testutil/containertest"
)

func TestRuntimeStatusMethods(t *testing.T) {
	t.Parallel()

	forEachHealthyRuntime(t, func(t *testing.T, ctx context.Context, runtime containertest.Runtime) {
		directStatus := runtime.Orchestrator.CheckStatus(ctx, containers.IgnoreCachedRuntimeStatus)
		require.True(t, directStatus.Installed)
		require.True(t, directStatus.Running)
		require.Empty(t, directStatus.Error)

		cachedStatus := runtime.Orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
		require.True(t, cachedStatus.Installed)
		require.True(t, cachedStatus.Running)
		require.Empty(t, cachedStatus.Error)

		diagnostics, diagnosticsErr := runtime.Orchestrator.GetDiagnostics(ctx)
		require.NoError(t, diagnosticsErr)
		require.True(t,
			diagnostics.ClientVersion != "" || diagnostics.ServerVersion != "",
			"expected runtime diagnostics to include at least one version",
		)
	})
}
