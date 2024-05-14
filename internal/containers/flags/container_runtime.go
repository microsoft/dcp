package flags

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/docker"
	"github.com/microsoft/usvc-apiserver/internal/podman"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/spf13/pflag"
)

type ContainerOrchestratorFactory func(log logr.Logger, executor process.Executor) containers.ContainerOrchestrator

type RuntimeFlagValue struct {
	Name                string
	OrchestratorFactory ContainerOrchestratorFactory
}

const RuntimeFlagName = "container-runtime"

var (
	supportedRuntimes = []*RuntimeFlagValue{
		{
			Name:                "docker",
			OrchestratorFactory: docker.NewDockerCliOrchestrator,
		},
		{
			Name:                "podman",
			OrchestratorFactory: podman.NewPodmanCliOrchestrator,
		},
	}
	supportedRuntimeNames                   = []string{}
	runtime               *RuntimeFlagValue = nil
	runtimeFlagValue                        = ""
)

func EnsureRuntimeFlag(flags *pflag.FlagSet) {
	flags.Var(runtime, RuntimeFlagName, fmt.Sprintf("The container runtime to use (%s)", strings.Join(supportedRuntimeNames, ", ")))
}

func GetRuntimeFlag() string {
	return "--" + RuntimeFlagName
}

func GetRuntimeFlagArg() string {
	return runtimeFlagValue
}

type runtimeSupport struct {
	runtime *RuntimeFlagValue
	status  containers.ContainerRuntimeStatus
}

func EnsureValidRuntimeFlagArgValue(ctx context.Context, log logr.Logger, executor process.Executor) error {
	// Write debug logs
	log = log.V(1)

	if runtime == nil && runtimeFlagValue != "" {
		// If the user specified a runtime but it wasn't valid, return an error
		return fmt.Errorf("container runtime \"%s\" is invalid, must be one of (%s)", runtimeFlagValue, strings.Join(supportedRuntimeNames, ", "))
	} else if runtime == nil && len(supportedRuntimes) == 0 {
		// If this build of DCP doesn't support ANY runtimes, report an error (this should never happen)
		return fmt.Errorf("no container runtimes are supported")
	} else if runtime == nil && len(supportedRuntimes) == 1 {
		runtime = supportedRuntimes[0]
	} else if runtime == nil && len(supportedRuntimes) > 1 {
		// If the user didn't specify a runtime, pick a supported runtime and use it
		runtimesCh := make(chan runtimeSupport, len(supportedRuntimes))

		for i := range supportedRuntimes {
			// Check each supported runtime to see if it's installed and running
			go func(supportedRuntime *RuntimeFlagValue) {
				orchestrator := supportedRuntime.OrchestratorFactory(log, executor)
				status := orchestrator.CheckStatus(ctx, true)
				log.Info("runtime status", "runtime", supportedRuntime.Name, "status", status)
				runtimesCh <- runtimeSupport{supportedRuntime, status}
			}(supportedRuntimes[i])
		}

		var availableRuntime *RuntimeFlagValue
		for i := 0; i < len(supportedRuntimes); i++ {
			supportedRuntime := <-runtimesCh
			if supportedRuntime.status.IsRunning() && supportedRuntime.runtime == supportedRuntimes[0] {
				log.Info("default runtime available", "runtime", supportedRuntime.runtime.Name)
				// If the first (default) runtime is available, use it
				availableRuntime = supportedRuntime.runtime
				break
			}

			if availableRuntime == nil && supportedRuntime.status.IsRunning() {
				log.Info("found valid runtime", "runtime", supportedRuntime.runtime.Name)
				availableRuntime = supportedRuntime.runtime
			}
		}

		if availableRuntime != nil {
			runtime = availableRuntime
		} else {
			runtime = supportedRuntimes[0]
		}
	}

	if runtime == nil {
		return fmt.Errorf("could not find a valid container runtime")
	}

	runtimeFlagValue = runtime.Name

	log.Info(runtime.Name)

	return nil
}

func GetContainerOrchestrator(ctx context.Context, log logr.Logger, executor process.Executor) (containers.ContainerOrchestrator, error) {
	if err := EnsureValidRuntimeFlagArgValue(ctx, log, executor); err != nil {
		return nil, err
	}

	// The user specified a specific runtime, so use it
	return runtime.OrchestratorFactory(log, executor), nil
}

func (rfv *RuntimeFlagValue) Set(flagValue string) error {
	for _, supportedRuntime := range supportedRuntimes {
		if supportedRuntime.Name == strings.ToLower(flagValue) {
			runtime = supportedRuntime
			runtimeFlagValue = supportedRuntime.Name

			return nil
		}
	}

	// We didn't match a valid runtime, so set selected runtime to nil
	runtimeFlagValue = strings.ToLower(flagValue)
	return nil
}

func (rfv *RuntimeFlagValue) String() string {
	if rfv == nil {
		return ""
	}

	return rfv.Name
}

func (rfv *RuntimeFlagValue) Type() string {
	return RuntimeFlagName
}

func init() {
	for _, supportedRuntime := range supportedRuntimes {
		supportedRuntimeNames = append(supportedRuntimeNames, supportedRuntime.Name)
	}
}
