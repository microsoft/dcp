/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/pkg/commonapi"
)

func TestStartApiServerRejectsTooLongEnvironmentWorkloadIDBeforeDetach(t *testing.T) {
	resetStartApiServerGlobals(t)
	cmd, cmdErr := NewStartApiSrvCommand(logr.Discard())
	require.NoError(t, cmdErr)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Flags().Set("detach", "true"))
	t.Setenv(cmds.DCPWorkloadIDEnvVar, strings.Repeat("a", commonapi.MaxWorkloadIDLength+1))

	startErr := cmd.RunE(cmd, nil)

	require.ErrorContains(t, startErr, "invalid DCP_WORKLOAD_ID")
	require.ErrorContains(t, startErr, "workload ID cannot be longer than")
}

func resetStartApiServerGlobals(t *testing.T) {
	t.Helper()

	originalRootDir := rootDir
	originalDetach := detach
	originalServerOnly := serverOnly
	rootDir = ""
	detach = false
	serverOnly = false
	t.Cleanup(func() {
		rootDir = originalRootDir
		detach = originalDetach
		serverOnly = originalServerOnly
	})
}
