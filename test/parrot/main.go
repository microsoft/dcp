// Copyright (c) Microsoft Corporation. All rights reserved.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "parrot",
	Short: "A network connectivity testing tool",
	Long:  `Parrot is a TCP-based network connectivity testing tool that can operate in client or server mode.`,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
