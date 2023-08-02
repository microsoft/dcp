package dcpctrl

import (
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/microsoft/usvc-apiserver/pkg/logger"
)

var (
	rootCmdLogger      logr.Logger
	rootCmdFlushLogger func()
)

func NewRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "dcpctrl",
		Short: "Runs standard DCP controllers (for Executable, Container, and ContainerVolume objects)",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you an development environment
	with minimum remote dependencies and maximum ease of use.

	dcpctrl is the host process that runs the controllers for the DCP API objects.`,
		SilenceUsage: true,
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			rootCmdFlushLogger()
		},
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	rootCmd.AddCommand(NewGetCapabilitiesCommand())
	rootCmd.AddCommand(NewRunControllersCommand())

	rootCmdLogger, rootCmdFlushLogger = logger.NewLogger(rootCmd.PersistentFlags())
	ctrlruntime.SetLogger(rootCmdLogger)

	return rootCmd
}
