package runtimes

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/docker"
	"github.com/microsoft/usvc-apiserver/internal/podman"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type ContainerOrchestratorFactory func(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator

var (
	errNoRuntimeFound = fmt.Errorf("no container runtime was found")
	supportedRuntimes = map[flags.RuntimeFlagValue]ContainerOrchestratorFactory{
		flags.DockerRuntime: docker.NewDockerCliOrchestrator,
		flags.PodmanRuntime: podman.NewPodmanCliOrchestrator,
	}
)

type runtimeSupport struct {
	orchestrator containers.ContainerOrchestrator
	status       containers.ContainerRuntimeStatus
}

func FindAvailableContainerRuntime(ctx context.Context, log logr.Logger, executor process.Executor) (containers.ContainerOrchestrator, error) {
	runtimeFlagValue := flags.GetRuntimeFlagValue()

	var availableRuntime *runtimeSupport
	if runtimeFlagValue == flags.UnknownRuntime {
		// If the user didn't specify a runtime, pick a supported runtime and use it
		runtimesCh := make(chan *runtimeSupport, len(supportedRuntimes))

		for _, runtimeFactory := range supportedRuntimes {
			// Check each supported runtime to see if it's installed and running
			go func(factory ContainerOrchestratorFactory) {
				orchestrator := factory(log, executor)
				status := orchestrator.CheckStatus(ctx, containers.IgnoreCachedRuntimeStatus)
				runtimesCh <- &runtimeSupport{orchestrator, status}
			}(runtimeFactory)
		}

		for i := 0; i < len(supportedRuntimes); i++ {
			supportedRuntime := <-runtimesCh

			switch {
			case availableRuntime == nil:
				// We haven't picked a runtime yet
				availableRuntime = supportedRuntime
			case !availableRuntime.status.Installed && supportedRuntime.status.Installed:
				// Prefer a runtime that is installed over one that isn't
				availableRuntime = supportedRuntime
			case !availableRuntime.status.Running && supportedRuntime.status.Running:
				// Prefer a runtime that is running over one that isn't
				availableRuntime = supportedRuntime
			case supportedRuntime.orchestrator.IsDefault() && supportedRuntime.status.Installed == availableRuntime.status.Installed && supportedRuntime.status.Running == availableRuntime.status.Running:
				// Prefer the default runtime
				availableRuntime = supportedRuntime
			}
		}
	} else {
		orchestratorFactory := supportedRuntimes[runtimeFlagValue]
		if orchestratorFactory != nil {
			orchestrator := orchestratorFactory(log, executor)
			status := orchestrator.CheckStatus(ctx, containers.IgnoreCachedRuntimeStatus)
			availableRuntime = &runtimeSupport{
				orchestrator,
				status,
			}
		}
	}

	if availableRuntime == nil {
		return nil, errNoRuntimeFound
	}

	log.V(1).Info("Runtime status", "Runtime", availableRuntime.orchestrator.Name(), "Status", availableRuntime.status)

	return availableRuntime.orchestrator, nil
}
