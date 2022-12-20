package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/usvc-dev/apiserver/internal/dcp/commands"
)

var rootCmd = &cobra.Command{
	Use:   "dcp",
	Short: "Runs and manages multi-service application and their dependencies",
	Long: `DCP is a developer tool for running multi-service applications. 
It integrates your code, emulators and containers to give you an development environment
with minimum remote dependencies and maximum ease of use.`,
}

func main() {
	root := commands.NewRootCmd()
	err := root.Execute()
	if err != nil {
		os.Exit(1)
	}
}
