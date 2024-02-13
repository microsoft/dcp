package flags

import (
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
	runtime          = &supportedRuntimes[0]
	runtimeFlagValue = supportedRuntimes[0].Name
)

func EnsureRuntimeFlag(flags *pflag.FlagSet) {
	flags.Var(runtime, RuntimeFlagName, "The container runtime to use (docker or podman)")
}

func GetRuntimeFlag() string {
	return "--" + RuntimeFlagName
}

func GetRuntimeFlagArg() string {
	return runtimeFlagValue
}

func EnsureValidRuntimeFlagArgValue() error {
	if runtime == nil {
		supportedRuntimeNames := []string{}
		for _, supportedRuntime := range supportedRuntimes {
			supportedRuntimeNames = append(supportedRuntimeNames, supportedRuntime.Name)
		}

		return fmt.Errorf("container runtime \"%s\" is invalid, must be one of (\"%s\")", runtimeFlagValue, strings.Join(supportedRuntimeNames, "\", \""))
	}

	return nil
}

func GetContainerOrchestrator(log logr.Logger, executor process.Executor) (containers.ContainerOrchestrator, error) {
	if err := EnsureValidRuntimeFlagArgValue(); err != nil {
		return nil, err
	}

	return runtime.OrchestratorFactory(log, executor), nil
}

func (rfv *RuntimeFlagValue) Set(flagValue string) error {
	for _, supportedRuntime := range supportedRuntimes {
		if supportedRuntime.Name == strings.ToLower(flagValue) {
			*rfv = supportedRuntime
			runtimeFlagValue = supportedRuntime.Name

			return nil
		}
	}

	// We didn't match a valid runtime, so set selected runtime to nil
	runtime = nil
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
