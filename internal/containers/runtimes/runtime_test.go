/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package runtimes

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/containers/flags"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

type selectionTestOrchestrator struct {
	containers.ContainerOrchestrator
	name   string
	status containers.ContainerRuntimeStatus
}

func (orchestrator selectionTestOrchestrator) Name() string {
	return orchestrator.name
}

func (orchestrator selectionTestOrchestrator) CheckStatus(context.Context, containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
	return orchestrator.status
}

func TestRuntimeSelectionPriorities(t *testing.T) {
	t.Parallel()

	absent := containers.ContainerRuntimeStatus{}
	stopped := containers.ContainerRuntimeStatus{Installed: true}
	healthy := containers.ContainerRuntimeStatus{Installed: true, Running: true}

	for _, testCase := range []struct {
		name   string
		docker containers.ContainerRuntimeStatus
		podman containers.ContainerRuntimeStatus
		wslc   containers.ContainerRuntimeStatus
		want   string
	}{
		{name: "all healthy", docker: healthy, podman: healthy, wslc: healthy, want: "docker"},
		{name: "podman and wslc", docker: absent, podman: healthy, wslc: healthy, want: "podman"},
		{name: "wslc only", docker: absent, podman: absent, wslc: healthy, want: "wslc"},
		{name: "wslc healthy others stopped", docker: stopped, podman: stopped, wslc: healthy, want: "wslc"},
		{name: "podman healthy docker stopped", docker: stopped, podman: healthy, wslc: healthy, want: "podman"},
		{name: "docker healthy wslc stopped", docker: healthy, podman: absent, wslc: stopped, want: "docker"},
		{name: "all stopped", docker: stopped, podman: stopped, wslc: stopped, want: "docker"},
		{name: "installed podman and wslc", docker: absent, podman: stopped, wslc: stopped, want: "podman"},
		{name: "only wslc installed", docker: absent, podman: absent, wslc: stopped, want: "wslc"},
		{name: "none installed", docker: absent, podman: absent, wslc: absent, want: "docker"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			candidates := []*runtimeSupport{
				{orchestrator: selectionTestOrchestrator{name: "docker"}, status: testCase.docker},
				{orchestrator: selectionTestOrchestrator{name: "podman"}, status: testCase.podman},
				{orchestrator: selectionTestOrchestrator{name: "wslc"}, status: testCase.wslc},
			}
			for _, order := range [][3]int{
				{0, 1, 2}, {0, 2, 1}, {1, 0, 2},
				{1, 2, 0}, {2, 0, 1}, {2, 1, 0},
			} {
				var selected *runtimeSupport
				for _, index := range order {
					selected = preferredRuntime(selected, candidates[index])
				}
				require.NotNil(t, selected)
				require.Equal(t, testCase.want, selected.orchestrator.Name(), "discovery order: %v", order)
			}
		})
	}
}

func TestRegisteredWSLCFactory(t *testing.T) {
	t.Parallel()

	executor := internal_testutil.NewTestProcessExecutor(t.Context())
	t.Cleanup(func() { require.NoError(t, executor.Close()) })
	factory := supportedRuntimes[flags.WslcRuntime]
	require.NotNil(t, factory)

	orchestrator := factory(logr.Discard(), executor)
	require.Equal(t, "wslc", orchestrator.Name())
	require.False(t, orchestrator.IsDefault())
	require.Empty(t, orchestrator.ContainerHost())
}

func TestExplicitWSLCSelectionDoesNotFallBack(t *testing.T) {
	originalFactories := supportedRuntimes
	originalRuntime := flags.GetRuntimeFlagValue()
	flagSet := pflag.NewFlagSet(t.Name(), pflag.ContinueOnError)
	flags.EnsureRuntimeFlag(flagSet)
	t.Cleanup(func() {
		supportedRuntimes = originalFactories
		require.NoError(t, flagSet.Set(flags.RuntimeFlagName, string(originalRuntime)))
	})
	require.NoError(t, flagSet.Set(flags.RuntimeFlagName, "wslc"))

	for _, healthy := range []bool{true, false} {
		var factoryCalls atomic.Int32
		supportedRuntimes = make(map[flags.RuntimeFlagValue]ContainerOrchestratorFactory)
		for _, runtimeName := range []flags.RuntimeFlagValue{flags.DockerRuntime, flags.PodmanRuntime, flags.WslcRuntime} {
			supportedRuntimes[runtimeName] = func(logr.Logger, process.Executor) containers.ContainerOrchestrator {
				factoryCalls.Add(1)
				return selectionTestOrchestrator{
					name: string(runtimeName),
					status: containers.ContainerRuntimeStatus{
						Installed: true,
						Running:   runtimeName != flags.WslcRuntime || healthy,
					},
				}
			}
		}

		orchestrator, findErr := FindAvailableContainerRuntime(t.Context(), logr.Discard(), nil)
		require.NoError(t, findErr)
		require.Equal(t, "wslc", orchestrator.Name())
		require.Equal(t, healthy, orchestrator.CheckStatus(t.Context(), containers.IgnoreCachedRuntimeStatus).IsHealthy())
		require.Equal(t, int32(1), factoryCalls.Load())
	}
}

func TestFindContainerRuntimeRejectsInvalidName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "  ", "unknown"} {
		orchestrator, findErr := FindContainerRuntime(context.Background(), name, logr.Discard(), nil)
		require.Error(t, findErr)
		require.Nil(t, orchestrator)
	}
}
