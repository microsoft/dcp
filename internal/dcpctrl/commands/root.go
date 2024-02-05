package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	"github.com/microsoft/usvc-apiserver/pkg/containers"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewRootCommand(logger logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		Use:   "dcpctrl",
		Short: "Runs standard DCP controllers (for Executable, Container, and ContainerVolume objects)",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you an development environment
	with minimum remote dependencies and maximum ease of use.

	dcpctrl is the host process that runs the controllers for the DCP API objects.`,
		SilenceUsage: true,
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	if cmd, err := cmds.NewVersionCommand(logger); err != nil {
		return nil, fmt.Errorf("could not set up 'version' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	rootCmd.AddCommand(NewGetCapabilitiesCommand(logger))
	rootCmd.AddCommand(NewRunControllersCommand(logger))

	containers.EnsureRuntimeFlag(rootCmd.PersistentFlags())

	logger.AddLevelFlag(rootCmd.PersistentFlags())
	ctrlruntime.SetLogger(logger.V(1))

	return rootCmd, nil
}
