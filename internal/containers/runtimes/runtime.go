/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package runtimes

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/internal/docker"
	"github.com/microsoft/dcp/internal/podman"
	"github.com/microsoft/dcp/internal/wslc"
	"github.com/microsoft/dcp/pkg/process"
)

type ContainerOrchestratorFactory func(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator

var (
	errNoRuntimeFound = fmt.Errorf("no container runtime was found")
	supportedRuntimes = map[flags.RuntimeFlagValue]ContainerOrchestratorFactory{
		flags.DockerRuntime: docker.NewDockerCliOrchestrator,
		flags.PodmanRuntime: podman.NewPodmanCliOrchestrator,
		flags.WslcRuntime:   wslc.NewWslcCliOrchestrator,
	}
)

type runtimeSupport struct {
	orchestrator containers.ContainerOrchestrator
	status       containers.ContainerRuntimeStatus
}

type runtimeProbeResult struct {
	index   int
	support *runtimeSupport
}

func FindAvailableContainerRuntime(ctx context.Context, log logr.Logger, executor process.Executor) (containers.ContainerOrchestrator, error) {
	runtimeFlagValue := flags.GetRuntimeFlagValue()

	var availableRuntime *runtimeSupport
	if runtimeFlagValue == flags.UnknownRuntime {
		// If the user didn't specify a runtime, pick a supported runtime and use it
		runtimeFactories := make([]ContainerOrchestratorFactory, 0, len(supportedRuntimes))
		for _, runtimeName := range []flags.RuntimeFlagValue{
			flags.DockerRuntime,
			flags.PodmanRuntime,
			flags.WslcRuntime,
		} {
			if runtimeFactory := supportedRuntimes[runtimeName]; runtimeFactory != nil {
				runtimeFactories = append(runtimeFactories, runtimeFactory)
			}
		}

		discoveryCtx, discoveryCancel := context.WithCancel(ctx)
		defer discoveryCancel()
		runtimesCh := make(chan runtimeProbeResult, len(runtimeFactories))
		runtimeResults := make([]*runtimeSupport, len(runtimeFactories))

		for index, runtimeFactory := range runtimeFactories {
			// Check each supported runtime to see if it's installed and running
			go func(resultIndex int, factory ContainerOrchestratorFactory) {
				orchestrator := factory(log, executor)
				status := orchestrator.CheckStatus(discoveryCtx, containers.IgnoreCachedRuntimeStatus)
				runtimesCh <- runtimeProbeResult{
					index:   resultIndex,
					support: &runtimeSupport{orchestrator, status},
				}
			}(index, runtimeFactory)
		}

		for range runtimeFactories {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case probeResult := <-runtimesCh:
				if probeResult.support == nil {
					return nil, fmt.Errorf("container runtime discovery returned an empty result")
				}
				runtimeResults[probeResult.index] = probeResult.support
				if selectedRuntime, selectionFinal := selectAvailableRuntime(runtimeResults); selectionFinal {
					discoveryCancel()
					return recordSelectedRuntime(log, selectedRuntime)
				}
			}
		}
	} else {
		orchestrator, runtimeErr := FindContainerRuntime(ctx, string(runtimeFlagValue), log, executor)
		if runtimeErr == nil {
			return orchestrator, nil
		}
	}

	if availableRuntime == nil {
		return nil, errNoRuntimeFound
	}

	return recordSelectedRuntime(log, availableRuntime)
}

func selectAvailableRuntime(runtimeResults []*runtimeSupport) (*runtimeSupport, bool) {
	var fallbackRuntime *runtimeSupport
	for _, runtimeResult := range runtimeResults {
		if runtimeResult == nil {
			return nil, false
		}
		if runtimeResult.status.IsHealthy() {
			return runtimeResult, true
		}
		if fallbackRuntime == nil ||
			(!fallbackRuntime.status.Installed && runtimeResult.status.Installed) ||
			(!fallbackRuntime.status.Running && runtimeResult.status.Running) {
			fallbackRuntime = runtimeResult
		}
	}
	return fallbackRuntime, true
}

func recordSelectedRuntime(log logr.Logger, availableRuntime *runtimeSupport) (containers.ContainerOrchestrator, error) {
	selectedRuntimeErr := flags.SetRuntimeFlagValue(flags.RuntimeFlagValue(availableRuntime.orchestrator.Name()))
	if selectedRuntimeErr != nil {
		return nil, fmt.Errorf("record selected container runtime: %w", selectedRuntimeErr)
	}

	log.V(1).Info("Runtime status", "Runtime", availableRuntime.orchestrator.Name(), "Status", availableRuntime.status)

	return availableRuntime.orchestrator, nil
}

func FindContainerRuntime(ctx context.Context, runtimeName string, log logr.Logger, executor process.Executor) (containers.ContainerOrchestrator, error) {
	runtimeName = strings.TrimSpace(strings.ToLower(runtimeName))
	if runtimeName == "" {
		return nil, fmt.Errorf("container runtime name cannot be empty")
	}

	orchestratorFactory := supportedRuntimes[flags.RuntimeFlagValue(runtimeName)]
	if orchestratorFactory == nil {
		return nil, fmt.Errorf("container runtime %q is not supported", runtimeName)
	}

	orchestrator := orchestratorFactory(log, executor)
	status := orchestrator.CheckStatus(ctx, containers.IgnoreCachedRuntimeStatus)
	log.V(1).Info("Runtime status", "Runtime", orchestrator.Name(), "Status", status)

	return orchestrator, nil
}
