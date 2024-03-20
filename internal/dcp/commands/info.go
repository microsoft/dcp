package commands

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/containers"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/internal/version"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

func NewInfoCommand(log logger.Logger) (*cobra.Command, error) {
	infoCmd := &cobra.Command{
		Use:   "info",
		Short: "Prints information about the application and its most important dependencies.",
		Long:  `Prints information.`,
		RunE:  getInfo(log),
		Args:  cobra.NoArgs,
	}

	container_flags.EnsureRuntimeFlag(infoCmd.PersistentFlags())

	return infoCmd, nil
}

type containerRuntime struct {
	// Name of the container runtime (i.e. docker, podman)
	Runtime string `json:"runtime"`

	// Default hostname within a container for accessing the host machine network
	HostName string `json:"hostName"`

	// Is the runtime installed?
	Installed bool `json:"installed"`

	// Is the runtime running?
	Running bool `json:"running"`

	// Error message if the runtime is not installed or not running
	Error string `json:"error,omitempty"`
}

type information struct {
	version.VersionOutput `json:",inline"`
	Containers            containerRuntime `json:"containers"`
}

func getInfo(log logger.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log := log.WithName("info")

		ctx := cmd.Context()

		processExecutor := process.NewOSExecutor(log)
		containerOrchestrator, orchestratorErr := container_flags.GetContainerOrchestrator(ctx, log, processExecutor)
		var status containers.ContainerRuntimeStatus
		if orchestratorErr != nil {
			status = containers.ContainerRuntimeStatus{
				Installed: false,
				Running:   false,
				Error:     orchestratorErr.Error(),
			}
		} else {
			status = containerOrchestrator.CheckStatus(ctx)
		}

		info := information{
			VersionOutput: version.Version(),
			Containers: containerRuntime{
				Runtime:   container_flags.GetRuntimeFlagArg(),
				HostName:  containerOrchestrator.ContainerHost(),
				Installed: status.Installed,
				Running:   status.Running,
				Error:     status.Error,
			},
		}

		if info, err := json.Marshal(info); err != nil {
			log.Error(err, "could not serialize application information")
			return err
		} else {
			fmt.Println(string(info))
		}

		return nil
	}
}
