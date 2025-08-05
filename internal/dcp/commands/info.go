package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/containers"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	container_runtimes "github.com/microsoft/usvc-apiserver/internal/containers/runtimes"
	"github.com/microsoft/usvc-apiserver/internal/version"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

const (
	defaultTimeout           = 10 * time.Second
	defaultDiagnosticTimeout = 90 * time.Second
)

var (
	// Default timeout for the info command
	timeout = defaultTimeout
	// Should additional diagnostic information be returned?
	diagnosticMode = false
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
	infoCmd.Flags().BoolVar(&diagnosticMode, "diagnostics", false, "Enable detailed diagnostic information")
	infoCmd.Flags().DurationVarP(&timeout, "timeout", "t", 0, "Duration before the command times out (default 10 seconds)")

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

	// Version of the container runtime client (diagnostic mode only)
	ClientVersion string `json:"clientVersion,omitempty"`

	// Version of the container runtime server (diagnostic mode only)
	ServerVersion string `json:"serverVersion,omitempty"`
}

type information struct {
	version.VersionOutput `json:",inline"`
	Containers            containerRuntime `json:"containers"`
	OS                    string           `json:"os,omitempty"`
	Arch                  string           `json:"architecture,omitempty"`
}

func getInfo(log logger.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log := log.WithName("info")

		if timeout <= 0 {
			if diagnosticMode {
				timeout = defaultDiagnosticTimeout
			} else {
				timeout = defaultTimeout
			}
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
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

		if diagnosticMode {
			// These values are hardcoded at build time, so no risk of PII exposure
			info.OS = runtime.GOOS
			info.Arch = runtime.GOARCH

			if status.Installed {
				diagnostics, diagnosticsErr := containerOrchestrator.GetDiagnostics(ctx)
				if diagnosticsErr != nil {
					log.Error(diagnosticsErr, "failed to get container runtime diagnostics")
				} else {
					info.Containers.ServerVersion = diagnostics.ServerVersion
					info.Containers.ClientVersion = diagnostics.ClientVersion
				}
			}
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
