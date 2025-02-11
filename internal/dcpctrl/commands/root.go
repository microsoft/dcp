package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	cmds "github.com/microsoft/usvc-apiserver/internal/commands"
	container_flags "github.com/microsoft/usvc-apiserver/internal/containers/flags"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

func NewRootCommand(log logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcpctrl",
		Short:         "Runs standard DCP controllers (for Executable, Container, and ContainerVolume objects)",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you an development environment
	with minimum remote dependencies and maximum ease of use.

	dcpctrl is the host process that runs the controllers for the DCP API objects.`,
		SilenceUsage:     true,
		PersistentPreRun: cmds.LogVersion(log, "Starting DCPCTRL..."),
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	var err error
	var cmd *cobra.Command

	if cmd, err = cmds.NewVersionCommand(log); err != nil {
		return nil, fmt.Errorf("could not set up 'version' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	rootCmd.AddCommand(NewGetCapabilitiesCommand(log))
	rootCmd.AddCommand(NewRunControllersCommand(log))

	container_flags.EnsureRuntimeFlag(rootCmd.PersistentFlags())

	log.AddLevelFlag(rootCmd.PersistentFlags())
	ctrlruntime.SetLogger(log.V(1))

	return rootCmd, nil
}
