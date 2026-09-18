/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package runtimes

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/pkg/process"
)

type testContainerOrchestrator struct {
	containers.ContainerOrchestrator
	name   string
	status containers.ContainerRuntimeStatus
}

func (o *testContainerOrchestrator) IsDefault() bool {
	return false
}

func (o *testContainerOrchestrator) Name() string {
	return o.name
}

func (o *testContainerOrchestrator) CheckStatus(context.Context, containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
	return o.status
}

func TestFindAvailableContainerRuntimeRecordsImplicitSelection(t *testing.T) {
	originalRuntime := flags.GetRuntimeFlagValue()
	originalSupportedRuntimes := supportedRuntimes
	t.Cleanup(func() {
		supportedRuntimes = originalSupportedRuntimes
		require.NoError(t, flags.SetRuntimeFlagValue(originalRuntime))
	})

	require.NoError(t, flags.SetRuntimeFlagValue(flags.UnknownRuntime))
	supportedRuntimes = map[flags.RuntimeFlagValue]ContainerOrchestratorFactory{
		flags.PodmanRuntime: func(logr.Logger, process.Executor) containers.ContainerOrchestrator {
			return &testContainerOrchestrator{
				name: string(flags.PodmanRuntime),
				status: containers.ContainerRuntimeStatus{
					Installed: true,
					Running:   true,
				},
			}
		},
	}

	orchestrator, findErr := FindAvailableContainerRuntime(context.Background(), logr.Discard(), nil)

	require.NoError(t, findErr)
	require.Equal(t, string(flags.PodmanRuntime), orchestrator.Name())
	require.Equal(t, flags.PodmanRuntime, flags.GetRuntimeFlagValue())
}
