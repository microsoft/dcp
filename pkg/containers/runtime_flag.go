package containers

import (
	"fmt"

	"github.com/go-logr/logr"
	internal "github.com/microsoft/usvc-apiserver/internal/containers"
	"github.com/microsoft/usvc-apiserver/internal/docker"
	"github.com/microsoft/usvc-apiserver/internal/podman"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/spf13/pflag"
)

type ContainerOrchestratorFactory func(log logr.Logger, executor process.Executor) internal.ContainerOrchestrator

type RuntimeFlagValue struct {
	Name                string
	OrchestratorFactory ContainerOrchestratorFactory
}

const ContainerRuntimeFlagName = "container-runtime"

var (
	supportedRuntimes = []RuntimeFlagValue{
		{
			Name:                "docker",
			OrchestratorFactory: docker.NewDockerCliOrchestrator,
		},
		{
			Name:                "podman",
			OrchestratorFactory: podman.NewPodmanCliOrchestrator,
		},
	}
	runtime = supportedRuntimes[0]
)

func EnsureRuntimeFlag(flags *pflag.FlagSet) {
	flags.Var(&runtime, ContainerRuntimeFlagName, "The container runtime to use (docker or podman)")
}

func GetRuntimeFlag() string {
	return "--" + ContainerRuntimeFlagName
}

func GetRuntimeFlagArg() string {
	return runtime.Name
}

func GetContainerOrchestrator(log logr.Logger, executor process.Executor) internal.ContainerOrchestrator {
	return runtime.OrchestratorFactory(log, executor)
}

func (rfv *RuntimeFlagValue) Set(flagValue string) error {
	for _, supportedRuntime := range supportedRuntimes {
		if supportedRuntime.Name == flagValue {
			*rfv = supportedRuntime

			return nil
		}
	}

	return fmt.Errorf("invalid runtime \"%s\", must be one of \"docker\" or \"podman\"", flagValue)
}

func (rfv *RuntimeFlagValue) String() string {
	return rfv.Name
}

func (rfv *RuntimeFlagValue) Type() string {
	return ContainerRuntimeFlagName
}
