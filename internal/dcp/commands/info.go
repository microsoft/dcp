package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/containers"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	container_runtimes "github.com/microsoft/usvc-apiserver/internal/containers/runtimes"
	"github.com/microsoft/usvc-apiserver/internal/version"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

// Default timeout for the info command
var timeout = 10

func NewInfoCommand(log logger.Logger) (*cobra.Command, error) {
	infoCmd := &cobra.Command{
		Use:   "info",
		Short: "Prints information about the application and its most important dependencies.",
		Long:  `Prints information.`,
		RunE:  getInfo(log),
		Args:  cobra.NoArgs,
	}

	container_flags.EnsureRuntimeFlag(infoCmd.PersistentFlags())
	infoCmd.Flags().IntVarP(&timeout, "timeout", "t", 10, "Timeout for the command in seconds")

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

		ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(timeout)*time.Second)
		defer cancel()

		processExecutor := process.NewOSExecutor(log)
		defer processExecutor.Dispose()

		containerOrchestrator, orchestratorErr := container_runtimes.FindAvailableContainerRuntime(ctx, log, processExecutor)
		var status containers.ContainerRuntimeStatus
		containerHost := ""
		orchestratorName := ""
		if orchestratorErr != nil {
			status = containers.ContainerRuntimeStatus{
				Installed: false,
				Running:   false,
				Error:     orchestratorErr.Error(),
			}
		} else {
			orchestratorName = containerOrchestrator.Name()
			status = containerOrchestrator.CheckStatus(ctx, containers.CachedRuntimeStatusAllowed)
			containerHost = containerOrchestrator.ContainerHost()
		}

		info := information{
			VersionOutput: version.Version(),
			Containers: containerRuntime{
				Runtime:   orchestratorName,
				HostName:  containerHost,
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
