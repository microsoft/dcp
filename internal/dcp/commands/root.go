package commands

import (
	"fmt"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	"github.com/usvc-dev/apiserver/pkg/logger"
)

var (
	rootCmdLogger      logr.Logger
	rootCmdFlushLogger func()
)

func NewRootCmd() (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		Use:   "dcp",
		Short: "Runs and manages multi-service applications and their dependencies",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you a development environment
	with minimum remote dependencies and maximum ease of use.`,
		SilenceUsage: true,
		PersistentPostRun: func(_ *cobra.Command, _ []string) {
			rootCmdFlushLogger()
		},
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	var err error
	var cmd *cobra.Command

	if cmd, err = NewGenerateFileCommand(); cmd != nil {
		rootCmd.AddCommand(cmd)
	} else {
		return nil, fmt.Errorf("Could not set up 'generate-file' command: %w", err)
	}

	if cmd, err = NewUpCommand(); cmd != nil {
		rootCmd.AddCommand(cmd)
	} else {
		return nil, fmt.Errorf("Could not set up 'up' command: %w", err)
	}

	if cmd, err = NewStartApiSrvCommand(); cmd != nil {
		rootCmd.AddCommand(cmd)
	} else {
		return nil, fmt.Errorf("Could not set up 'start-apiserver' command: %w", err)
	}

	rootCmdLogger, rootCmdFlushLogger = logger.NewLogger(rootCmd.PersistentFlags())
	ctrlruntime.SetLogger(rootCmdLogger)

	return rootCmd, nil
}
