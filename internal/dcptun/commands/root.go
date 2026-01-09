/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	container_flags "github.com/microsoft/dcp/internal/containers/flags"
	"github.com/microsoft/dcp/pkg/logger"
)

func NewRootCommand(logger *logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcptun",
		Short:         "Runs reverse tunnels for DCP applications",
		Long: `Runs reverse tunnels for DCP applications.

	A reverse tunnel allows clients to connect to servers running on a different network that is not diredctly accessible for clients`,
		SilenceUsage:     true,
		PersistentPreRun: cmds.LogVersion(logger.Logger, "Starting DCPTUN..."),
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	logger.AddLevelFlag(rootCmd.PersistentFlags())
	container_flags.EnsureRuntimeFlag(rootCmd.PersistentFlags())

	var err error
	var cmd *cobra.Command

	if cmd, err = cmds.NewVersionCommand(logger.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'version' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	rootCmd.AddCommand(NewRunServerCommand(logger.Logger))
	rootCmd.AddCommand(NewRunClientCommand(logger.Logger))

	return rootCmd, nil
}
