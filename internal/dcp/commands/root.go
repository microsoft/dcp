package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

func NewRootCmd() (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		Use:   "dcp",
		Short: "Runs and manages multi-service application and their dependencies",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you an development environment
	with minimum remote dependencies and maximum ease of use.`,
		SilenceUsage: true,
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
		return nil, fmt.Errorf("Could not set up 'generate-file' command: %w", err)
	}

	return rootCmd, nil
}
