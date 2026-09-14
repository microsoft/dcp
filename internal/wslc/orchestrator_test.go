/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
)

func TestOrchestratorIdentity(t *testing.T) {
	t.Parallel()

	_, orchestrator, _ := newTestOrchestrator(t)
	require.Equal(t, "wslc", orchestrator.Name())
	require.False(t, orchestrator.IsDefault())
	require.Empty(t, orchestrator.ContainerHost())
	require.Equal(t, "bridge", orchestrator.DefaultNetworkName())
}

func TestStatusHealthyWithDefaultSession(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"}}`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"},"Server":{"SessionManagerVersion":"2.9.11","Sessions":[{"ID":1,"Name":"default"}]}}`,
		"",
		0,
	)

	status := orchestrator.getStatusForOS(ctx, "windows")

	require.True(t, status.Installed)
	require.True(t, status.Running)
	require.Empty(t, status.Error)
}

func TestStatusDistinguishesInstalledFromRunning(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"}}`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"},"Server":{"SessionManagerVersion":"2.9.11","Sessions":[]}}`,
		"",
		0,
	)

	status := orchestrator.getStatusForOS(ctx, "windows")

	require.True(t, status.Installed)
	require.False(t, status.Running)
	require.Contains(t, status.Error, "no default runtime session")
}

func TestStatusRejectsInvalidVersionOutputAsNotInstalled(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "version", "--format", "json"},
		`{"Client":{}}`,
		"",
		0,
	)

	status := orchestrator.getStatusForOS(ctx, "windows")

	require.False(t, status.Installed)
	require.False(t, status.Running)
	require.Contains(t, status.Error, "did not contain a client version")
	require.Empty(t, executor.FindAll([]string{"wslc", "info"}, "", nil))
}

func TestStatusReportsMissingExecutableAsNotInstalled(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	executor.InstallAutoExecution(internal_testutil.AutoExecution{
		Condition: internal_testutil.ProcessSearchCriteria{
			Command: []string{"wslc", "version", "--format", "json"},
		},
		StartupError: func(*internal_testutil.ProcessExecution) error {
			return exec.ErrNotFound
		},
	})

	status := orchestrator.getStatusForOS(ctx, "windows")

	require.False(t, status.Installed)
	require.False(t, status.Running)
	require.NotEmpty(t, status.Error)
}

func TestStatusAndDiagnosticsAreUnavailableOffWindows(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)

	status := orchestrator.getStatusForOS(ctx, "linux")
	diagnostics, diagnosticsErr := orchestrator.getDiagnosticsForOS(ctx, "linux")

	require.False(t, status.Installed)
	require.False(t, status.Running)
	require.Contains(t, status.Error, "Windows")
	require.Empty(t, diagnostics)
	require.ErrorContains(t, diagnosticsErr, "non-Windows")
	require.Empty(t, executor.Executions)
}

func TestDiagnosticsDecodeClientAndSessionManagerVersions(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"},"Server":{"SessionManagerVersion":"2.9.11","Sessions":[{"ID":1}]}}`,
		"",
		0,
	)

	diagnostics, diagnosticsErr := orchestrator.getDiagnosticsForOS(ctx, "windows")

	require.NoError(t, diagnosticsErr)
	require.Equal(t, "2.9.11.0", diagnostics.ClientVersion)
	require.Equal(t, "2.9.11", diagnostics.ServerVersion)
}

func TestStatusCacheIsInstanceScoped(t *testing.T) {
	t.Parallel()

	ctxOne, orchestratorOne, executorOne := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executorOne,
		[]string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"}}`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executorOne,
		[]string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"},"Server":{"SessionManagerVersion":"2.9.11","Sessions":[{"ID":1}]}}`,
		"",
		0,
	)
	statusOne := orchestratorOne.CheckStatus(ctxOne, containers.CachedRuntimeStatusAllowed)
	require.True(t, statusOne.IsHealthy())

	ctxTwo, orchestratorTwo, executorTwo := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executorTwo,
		[]string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"}}`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executorTwo,
		[]string{"wslc", "info", "--format", "json"},
		"",
		"session manager unavailable",
		1,
	)
	statusTwo := orchestratorTwo.CheckStatus(ctxTwo, containers.CachedRuntimeStatusAllowed)

	require.True(t, statusTwo.Installed)
	require.False(t, statusTwo.Running)
	require.True(t, orchestratorOne.CheckStatus(ctxOne, containers.CachedRuntimeStatusAllowed).IsHealthy())
}

func TestBackgroundStatusUpdatesAreIdempotent(t *testing.T) {
	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"}}`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"},"Server":{"SessionManagerVersion":"2.9.11","Sessions":[{"ID":1}]}}`,
		"",
		0,
	)

	orchestrator.EnsureBackgroundStatusUpdates(ctx)
	orchestrator.EnsureBackgroundStatusUpdates(ctx)
	_, waitErr := internal_testutil.WaitForCommand(
		executor,
		ctx,
		[]string{"wslc", "info", "--format", "json"},
		"",
		nil,
	)
	require.NoError(t, waitErr)
	require.Len(t, executor.FindAll([]string{"wslc", "version", "--format", "json"}, "", nil), 1)
	require.Len(t, executor.FindAll([]string{"wslc", "info", "--format", "json"}, "", nil), 1)
	require.True(t, orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed).IsHealthy())
}

func TestWatchMethodsReturnExplicitUnsupportedErrors(t *testing.T) {
	t.Parallel()

	_, orchestrator, executor := newTestOrchestrator(t)
	containerSubscription, containerErr := orchestrator.WatchContainers(make(chan containers.EventMessage))
	networkSubscription, networkErr := orchestrator.WatchNetworks(make(chan containers.EventMessage))

	require.Nil(t, containerSubscription)
	require.ErrorContains(t, containerErr, "does not expose a native event stream")
	require.Nil(t, networkSubscription)
	require.ErrorContains(t, networkErr, "does not expose a native event stream")
	require.Empty(t, executor.Executions)
}
