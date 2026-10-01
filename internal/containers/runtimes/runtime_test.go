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
	name        string
	status      containers.ContainerRuntimeStatus
	checkStatus func(context.Context) containers.ContainerRuntimeStatus
}

func (orchestrator selectionTestOrchestrator) Name() string {
	return orchestrator.name
}

func (orchestrator selectionTestOrchestrator) CheckStatus(ctx context.Context, _ containers.CachedRuntimeStatusUsage) containers.ContainerRuntimeStatus {
	if orchestrator.checkStatus != nil {
		return orchestrator.checkStatus(ctx)
	}
	return orchestrator.status
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
			return &selectionTestOrchestrator{
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

// Verifies that runtime selection favors healthy and installed candidates, then Docker, Podman, and WSLC independently of discovery order.
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

// Verifies that the registered WSLC factory exposes its native container-to-host address.
func TestRegisteredWSLCFactory(t *testing.T) {
	t.Parallel()

	executor := internal_testutil.NewTestProcessExecutor(t.Context())
	t.Cleanup(func() { require.NoError(t, executor.Close()) })
	factory := supportedRuntimes[flags.WslcRuntime]
	require.NotNil(t, factory)

	orchestrator := factory(logr.Discard(), executor)
	require.Equal(t, "wslc", orchestrator.Name())
	require.Equal(t, "host.wslc.internal", orchestrator.ContainerHost())
}

// Verifies that a healthy highest-priority runtime returns immediately and cancels lower-priority probes.
func TestFindAvailableContainerRuntimeCancelsLowerPriorityProbes(t *testing.T) {
	originalRuntime := flags.GetRuntimeFlagValue()
	originalSupportedRuntimes := supportedRuntimes
	t.Cleanup(func() {
		supportedRuntimes = originalSupportedRuntimes
		require.NoError(t, flags.SetRuntimeFlagValue(originalRuntime))
	})
	require.NoError(t, flags.SetRuntimeFlagValue(flags.UnknownRuntime))

	lowerProbeStarted := make(chan struct{})
	lowerProbeCancelled := make(chan struct{})
	supportedRuntimes = map[flags.RuntimeFlagValue]ContainerOrchestratorFactory{
		flags.DockerRuntime: func(logr.Logger, process.Executor) containers.ContainerOrchestrator {
			return selectionTestOrchestrator{
				name:   string(flags.DockerRuntime),
				status: containers.ContainerRuntimeStatus{Installed: true, Running: true},
			}
		},
		flags.WslcRuntime: func(logr.Logger, process.Executor) containers.ContainerOrchestrator {
			return selectionTestOrchestrator{
				name: string(flags.WslcRuntime),
				checkStatus: func(ctx context.Context) containers.ContainerRuntimeStatus {
					close(lowerProbeStarted)
					<-ctx.Done()
					close(lowerProbeCancelled)
					return containers.ContainerRuntimeStatus{Installed: true, Error: ctx.Err().Error()}
				},
			}
		},
	}

	type findResult struct {
		orchestrator containers.ContainerOrchestrator
		err          error
	}
	result := make(chan findResult, 1)
	go func() {
		orchestrator, findErr := FindAvailableContainerRuntime(t.Context(), logr.Discard(), nil)
		result <- findResult{orchestrator: orchestrator, err: findErr}
	}()

	select {
	case <-lowerProbeStarted:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	var selected findResult
	select {
	case selected = <-result:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	require.NoError(t, selected.err)
	require.Equal(t, string(flags.DockerRuntime), selected.orchestrator.Name())
	select {
	case <-lowerProbeCancelled:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

// Verifies that a healthy runtime is not selected until all higher-priority probes are resolved.
func TestFindAvailableContainerRuntimeWaitsForHigherPriorityProbe(t *testing.T) {
	originalRuntime := flags.GetRuntimeFlagValue()
	originalSupportedRuntimes := supportedRuntimes
	t.Cleanup(func() {
		supportedRuntimes = originalSupportedRuntimes
		require.NoError(t, flags.SetRuntimeFlagValue(originalRuntime))
	})
	require.NoError(t, flags.SetRuntimeFlagValue(flags.UnknownRuntime))

	dockerStarted := make(chan struct{})
	releaseDocker := make(chan struct{})
	podmanCompleted := make(chan struct{})
	wslcCancelled := make(chan struct{})
	supportedRuntimes = map[flags.RuntimeFlagValue]ContainerOrchestratorFactory{
		flags.DockerRuntime: func(logr.Logger, process.Executor) containers.ContainerOrchestrator {
			return selectionTestOrchestrator{
				name: string(flags.DockerRuntime),
				checkStatus: func(context.Context) containers.ContainerRuntimeStatus {
					close(dockerStarted)
					<-releaseDocker
					return containers.ContainerRuntimeStatus{}
				},
			}
		},
		flags.PodmanRuntime: func(logr.Logger, process.Executor) containers.ContainerOrchestrator {
			return selectionTestOrchestrator{
				name: string(flags.PodmanRuntime),
				checkStatus: func(context.Context) containers.ContainerRuntimeStatus {
					close(podmanCompleted)
					return containers.ContainerRuntimeStatus{Installed: true, Running: true}
				},
			}
		},
		flags.WslcRuntime: func(logr.Logger, process.Executor) containers.ContainerOrchestrator {
			return selectionTestOrchestrator{
				name: string(flags.WslcRuntime),
				checkStatus: func(ctx context.Context) containers.ContainerRuntimeStatus {
					<-ctx.Done()
					close(wslcCancelled)
					return containers.ContainerRuntimeStatus{Installed: true, Error: ctx.Err().Error()}
				},
			}
		},
	}

	type findResult struct {
		orchestrator containers.ContainerOrchestrator
		err          error
	}
	result := make(chan findResult, 1)
	go func() {
		orchestrator, findErr := FindAvailableContainerRuntime(t.Context(), logr.Discard(), nil)
		result <- findResult{orchestrator: orchestrator, err: findErr}
	}()

	for _, ready := range []<-chan struct{}{dockerStarted, podmanCompleted} {
		select {
		case <-ready:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	select {
	case earlyResult := <-result:
		t.Fatalf("selected %v before Docker status was known", earlyResult.orchestrator)
	default:
	}

	close(releaseDocker)
	var selected findResult
	select {
	case selected = <-result:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	require.NoError(t, selected.err)
	require.Equal(t, string(flags.PodmanRuntime), selected.orchestrator.Name())
	select {
	case <-wslcCancelled:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

// Verifies that explicit WSLC selection invokes only its factory and never falls back to another runtime when WSLC is unhealthy.
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

// Verifies that empty, whitespace-only, and unknown runtime names return an error without an orchestrator.
func TestFindContainerRuntimeRejectsInvalidName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "  ", "unknown"} {
		orchestrator, findErr := FindContainerRuntime(context.Background(), name, logr.Discard(), nil)
		require.Error(t, findErr)
		require.Nil(t, orchestrator)
	}
}
