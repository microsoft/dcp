package commands

import (
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "dcp",
		Short: "Runs and manages multi-service application and their dependencies",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you an development environment
	with minimum remote dependencies and maximum ease of use.`,
		SilenceUsage: true,
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	rootCmd.AddCommand(NewGenerateFileCommand())
	rootCmd.AddCommand(NewUpCommand())

	return rootCmd
}
