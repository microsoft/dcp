/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	cmds "github.com/microsoft/dcp/internal/commands"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/pkg/logger"
)

func NewRootCommand(log *logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcpctrl",
		Short:         "Runs standard DCP controllers (for Executable, Container, and ContainerVolume objects)",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you an development environment
	with minimum remote dependencies and maximum ease of use.

	dcpctrl is the host process that runs the controllers for the DCP API objects.`,
		SilenceUsage:     true,
		PersistentPreRun: cmds.LogVersion(log.Logger, "Starting DCPCTRL..."),
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	log.AddLevelFlag(rootCmd.PersistentFlags())
	container_flags.EnsureRuntimeFlag(rootCmd.PersistentFlags())

	var err error
	var cmd *cobra.Command

	if cmd, err = cmds.NewVersionCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'version' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	rootCmd.AddCommand(NewGetCapabilitiesCommand(log.Logger))
	rootCmd.AddCommand(NewRunControllersCommand(log.Logger))

	ctrlruntime.SetLogger(log.V(1))

	return rootCmd, nil
}
