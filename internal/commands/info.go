package commands

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/microsoft/usvc-apiserver/internal/version"
	"github.com/microsoft/usvc-apiserver/pkg/containers"
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

	return infoCmd, nil
}

type containerRuntime struct {
	Runtime   string `json:"runtime"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Error     string `json:"error,omitempty"`
}

type information struct {
	Version    *version.VersionOutput `json:",inline"`
	Containers containerRuntime       `json:"containers"`
}

func getInfo(log logger.Logger) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log := log.WithName("info")

		ctx := cmd.Context()

		processExecutor := process.NewOSExecutor()
		status := containers.GetContainerOrchestrator(log, processExecutor).CheckStatus(ctx)

		info := information{
			Version: version.Version(),
			Containers: containerRuntime{
				Runtime:   containers.GetRuntimeFlagArg(),
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
