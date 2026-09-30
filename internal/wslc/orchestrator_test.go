/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/dcp/internal/containers"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
)

// Verifies that WSLC reports its runtime name, non-default selection, unsupported host address, and default bridge network.
func TestOrchestratorIdentity(t *testing.T) {
	t.Parallel()

	_, orchestrator, _ := newTestOrchestrator(t)
	require.Equal(t, "wslc", orchestrator.Name())
	require.Equal(t, "host.wslc.internal", orchestrator.ContainerHost())
	require.Equal(t, "bridge", orchestrator.DefaultNetworkName())
}

// Verifies that supported client and session-manager versions with an active session produce a healthy runtime status.
func TestStatusHealthyWithDefaultSession(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"3.0.1.0"}}`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"3.0.1.0"},"Server":{"SessionManagerVersion":"3.0.1","Sessions":[{"ID":1,"Name":"default"}]}}`,
		"",
		0,
	)

	status := orchestrator.getStatusForOS(ctx, "windows")

	require.True(t, status.Installed)
	require.True(t, status.Running)
	require.Empty(t, status.Error)
}

// Verifies that a responsive supported session manager is healthy before a default session is created.
func TestStatusAllowsLazyDefaultSessionCreation(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name     string
		sessions string
	}{
		{name: "no sessions", sessions: `[]`},
		{name: "named session only", sessions: `[{"ID":1,"Name":"named"}]`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, orchestrator, executor := newTestOrchestrator(t)
			installAutoCommand(
				t,
				executor,
				[]string{"wslc", "version", "--format", "json"},
				`{"Client":{"Version":"3.0.1.0"}}`,
				"",
				0,
			)
			installAutoCommand(
				t,
				executor,
				[]string{"wslc", "info", "--format", "json"},
				`{"Client":{"Version":"3.0.1.0"},"Server":{"SessionManagerVersion":"3.0.1","Sessions":`+
					testCase.sessions+`}}`,
				"",
				0,
			)

			status := orchestrator.getStatusForOS(ctx, "windows")

			require.True(t, status.Installed)
			require.True(t, status.Running)
			require.Empty(t, status.Error)
		})
	}
}

// Verifies that missing client-version data prevents WSLC from being reported as installed and avoids a session-status query.
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

// Verifies that failure to locate the WSLC executable produces an uninstalled, non-running status with an error.
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

// Verifies that non-Windows status and diagnostics reject WSLC without executing any CLI commands.
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

// Verifies that WSLC diagnostics preserve both client and session-manager version strings from native JSON.
func TestDiagnosticsDecodeClientAndSessionManagerVersions(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"3.0.1.0"},"Server":{"SessionManagerVersion":"3.0.1","Sessions":[{"ID":1}]}}`,
		"",
		0,
	)

	diagnostics, diagnosticsErr := orchestrator.getDiagnosticsForOS(ctx, "windows")

	require.NoError(t, diagnosticsErr)
	require.Equal(t, "3.0.1.0", diagnostics.ClientVersion)
	require.Equal(t, "3.0.1", diagnostics.ServerVersion)
}

// Verifies that cached healthy and unhealthy statuses remain instance-scoped and are read without executing CLI commands.
func TestStatusCacheIsInstanceScoped(t *testing.T) {
	t.Parallel()

	ctxOne, orchestratorOne, executorOne := newTestOrchestrator(t)
	healthy := containers.ContainerRuntimeStatus{Installed: true, Running: true}
	orchestratorOne.storeStatus(healthy)
	statusOne := orchestratorOne.CheckStatus(ctxOne, containers.CachedRuntimeStatusAllowed)
	require.Equal(t, healthy, statusOne)

	ctxTwo, orchestratorTwo, executorTwo := newTestOrchestrator(t)
	unhealthy := containers.ContainerRuntimeStatus{Installed: true, Error: "session manager unavailable"}
	orchestratorTwo.storeStatus(unhealthy)
	statusTwo := orchestratorTwo.CheckStatus(ctxTwo, containers.CachedRuntimeStatusAllowed)

	require.Equal(t, unhealthy, statusTwo)
	require.Equal(t, healthy, orchestratorOne.CheckStatus(ctxOne, containers.CachedRuntimeStatusAllowed))
	require.Empty(t, executorOne.Executions)
	require.Empty(t, executorTwo.Executions)
}

// Verifies that repeated background-update requests share one worker and cache the platform-appropriate status.
// Windows performs one native refresh, while other platforms report unavailability without invoking WSLC.
func TestBackgroundStatusUpdatesAreIdempotent(t *testing.T) {
	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"3.0.1.0"}}`,
		"",
		0,
	)
	installAutoCommand(
		t,
		executor,
		[]string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"3.0.1.0"},"Server":{"SessionManagerVersion":"3.0.1","Sessions":[{"ID":1}]}}`,
		"",
		0,
	)

	orchestrator.EnsureBackgroundStatusUpdates(ctx)
	orchestrator.EnsureBackgroundStatusUpdates(ctx)
	waitErr := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, func(context.Context) (bool, error) {
		orchestrator.statusLock.RLock()
		defer orchestrator.statusLock.RUnlock()
		return orchestrator.cachedStatus != nil, nil
	})
	require.NoError(t, waitErr)
	require.Equal(t, int32(1), orchestrator.statusWorker.Load())
	if runtime.GOOS != "windows" {
		require.Empty(t, executor.Executions)
		require.Contains(t, orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed).Error, "Windows")
		return
	}
	require.Len(t, executor.FindAll([]string{"wslc", "version", "--format", "json"}, "", nil), 1)
	require.Len(t, executor.FindAll([]string{"wslc", "info", "--format", "json"}, "", nil), 1)
	require.True(t, orchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed).IsHealthy())
}

// Verifies that an older WSLC client is installed but unusable and is rejected before querying the session manager.
func TestStatusRejectsUnsupportedClientBeforeCheckingSession(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(t, executor, []string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"2.9.11.0"}}`, "", 0)

	status := orchestrator.getStatusForOS(ctx, "windows")
	require.True(t, status.Installed)
	require.False(t, status.Running)
	require.Contains(t, status.Error, "requires WSLC 3.0.1.0 or newer")
	require.Empty(t, executor.FindAll([]string{"wslc", "info"}, "", nil))
}

// Verifies that a supported WSLC client cannot make an older session manager appear healthy.
func TestStatusRejectsUnsupportedSessionManager(t *testing.T) {
	t.Parallel()

	ctx, orchestrator, executor := newTestOrchestrator(t)
	installAutoCommand(t, executor, []string{"wslc", "version", "--format", "json"},
		`{"Client":{"Version":"3.0.1.0"}}`, "", 0)
	installAutoCommand(t, executor, []string{"wslc", "info", "--format", "json"},
		`{"Client":{"Version":"3.0.1.0"},"Server":{"SessionManagerVersion":"2.9.11","Sessions":[{"ID":1}]}}`, "", 0)

	status := orchestrator.getStatusForOS(ctx, "windows")
	require.True(t, status.Installed)
	require.False(t, status.Running)
	require.Contains(t, status.Error, "session manager version 2.9.11 is unsupported")
}

// Verifies SemVer release, prerelease, and metadata precedence plus WSLC's numeric revision format against the minimum version.
// Higher major or minor versions are accepted regardless of their lower components, while older and malformed versions are rejected.
func TestValidateWslcVersion(t *testing.T) {
	t.Parallel()

	for _, version := range []string{
		"3.0.1", "3.0.1.0", "3.0.1.1", "3.0.2", "3.0.2.0", "3.0.10",
		"3.1.0", "3.1.0.0", "3.10.0", "4.0.0", "4.0.0.0", "10.0.0",
		"3.0.1+build.7", "3.0.2-alpha.1", "4.0.0-rc.1",
	} {
		require.NoError(t, validateWslcVersion(version, "client"), version)
	}
	for _, version := range []string{
		"2.9.11.0", "2.99.99.99", "3.0.0", "3.0.0.99", "3.0.1-alpha.1",
		"3.0.1-rc.1+build.7", "", "3.0", "3.0.1.0.1", "3.0.1.x",
		"3.0.1.-1", "3.0.1.+1", "3.0.1.18446744073709551616",
	} {
		require.Error(t, validateWslcVersion(version, "client"), version)
	}
}
