/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package runtimes

import (
	"context"
	"fmt"
	"slices"
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
	name    flags.RuntimeFlagValue
	support *runtimeSupport
}

func FindAvailableContainerRuntime(ctx context.Context, log logr.Logger, executor process.Executor) (containers.ContainerOrchestrator, error) {
	runtimeFlagValue := flags.GetRuntimeFlagValue()

	var availableRuntime *runtimeSupport
	if runtimeFlagValue == flags.UnknownRuntime {
		// If the user didn't specify a runtime, pick a supported runtime and use it
		runtimeNames := registeredRuntimeNames()
		discoveryCtx, discoveryCancel := context.WithCancel(ctx)
		defer discoveryCancel()
		runtimesCh := make(chan runtimeProbeResult, len(runtimeNames))
		pendingRuntimes := make(map[flags.RuntimeFlagValue]struct{}, len(runtimeNames))

		for _, runtimeName := range runtimeNames {
			runtimeFactory := supportedRuntimes[runtimeName]
			pendingRuntimes[runtimeName] = struct{}{}
			// Check each supported runtime to see if it's installed and running
			go func(name flags.RuntimeFlagValue, factory ContainerOrchestratorFactory) {
				orchestrator := factory(log, executor)
				status := orchestrator.CheckStatus(discoveryCtx, containers.IgnoreCachedRuntimeStatus)
				runtimesCh <- runtimeProbeResult{
					name:    name,
					support: &runtimeSupport{orchestrator, status},
				}
			}(runtimeName, runtimeFactory)
		}

		for len(pendingRuntimes) > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case probeResult := <-runtimesCh:
				delete(pendingRuntimes, probeResult.name)
				if probeResult.support == nil {
					return nil, fmt.Errorf("container runtime discovery returned an empty result for %q", probeResult.name)
				}
				availableRuntime = preferredRuntime(availableRuntime, probeResult.support)
				if runtimeSelectionFinal(availableRuntime, pendingRuntimes) {
					discoveryCancel()
					return recordSelectedRuntime(log, availableRuntime)
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

func registeredRuntimeNames() []flags.RuntimeFlagValue {
	priorityOrder := []flags.RuntimeFlagValue{
		flags.DockerRuntime,
		flags.PodmanRuntime,
		flags.WslcRuntime,
	}
	runtimeNames := make([]flags.RuntimeFlagValue, 0, len(supportedRuntimes))
	registered := make(map[flags.RuntimeFlagValue]struct{}, len(priorityOrder))
	for _, runtimeName := range priorityOrder {
		if supportedRuntimes[runtimeName] != nil {
			runtimeNames = append(runtimeNames, runtimeName)
			registered[runtimeName] = struct{}{}
		}
	}
	extraRuntimeNames := make([]flags.RuntimeFlagValue, 0, len(supportedRuntimes)-len(runtimeNames))
	for runtimeName, runtimeFactory := range supportedRuntimes {
		if runtimeFactory == nil {
			continue
		}
		if _, found := registered[runtimeName]; !found {
			extraRuntimeNames = append(extraRuntimeNames, runtimeName)
		}
	}
	slices.Sort(extraRuntimeNames)
	runtimeNames = append(runtimeNames, extraRuntimeNames...)
	return runtimeNames
}

func runtimeSelectionFinal(
	availableRuntime *runtimeSupport,
	pendingRuntimes map[flags.RuntimeFlagValue]struct{},
) bool {
	if availableRuntime == nil || !availableRuntime.status.IsHealthy() {
		return false
	}
	selectedPriority := runtimePriority(availableRuntime.orchestrator.Name())
	for runtimeName := range pendingRuntimes {
		if runtimePriority(string(runtimeName)) < selectedPriority {
			return false
		}
	}
	return true
}

func recordSelectedRuntime(log logr.Logger, availableRuntime *runtimeSupport) (containers.ContainerOrchestrator, error) {
	selectedRuntimeErr := flags.SetRuntimeFlagValue(flags.RuntimeFlagValue(availableRuntime.orchestrator.Name()))
	if selectedRuntimeErr != nil {
		return nil, fmt.Errorf("record selected container runtime: %w", selectedRuntimeErr)
	}

	log.V(1).Info("Runtime status", "Runtime", availableRuntime.orchestrator.Name(), "Status", availableRuntime.status)

	return availableRuntime.orchestrator, nil
}

func preferredRuntime(current, candidate *runtimeSupport) *runtimeSupport {
	switch {
	case current == nil:
		return candidate
	case !current.status.Installed && candidate.status.Installed:
		return candidate
	case !current.status.Running && candidate.status.Running:
		return candidate
	case current.status.Installed == candidate.status.Installed &&
		current.status.Running == candidate.status.Running &&
		runtimePriority(candidate.orchestrator.Name()) < runtimePriority(current.orchestrator.Name()):
		return candidate
	default:
		return current
	}
}

func runtimePriority(runtimeName string) int {
	switch flags.RuntimeFlagValue(runtimeName) {
	case flags.DockerRuntime:
		return 0
	case flags.PodmanRuntime:
		return 1
	case flags.WslcRuntime:
		return 2
	default:
		return 3
	}
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
