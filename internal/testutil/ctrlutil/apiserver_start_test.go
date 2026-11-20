/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that ApiServerInfo.Dispose stops the API server process when a graceful shutdown cannot be
// requested, as happens when startup fails before the REST client is created.
func TestApiServerDisposeStopsServerWithoutGracefulShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	sessionFolder, sessionFolderErr := testutil.CreateTestSessionDir()
	require.NoError(t, sessionFolderErr)

	serverInfo, startErr := StartApiServer(ctx, ApiServerFlagsNone, testutil.NewLogForTesting(t.Name()), sessionFolder)
	require.NoError(t, startErr)
	serverInfo.RestClient = nil
	serverInfo.Dispose()

	select {
	case <-serverInfo.ApiServerExited.Wait():
	case <-ctx.Done():
		t.Fatal("Dispose did not stop the API server process")
	}
	require.NoDirExists(t, sessionFolder)
}
