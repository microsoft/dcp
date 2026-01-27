/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/pkg/logger"
)

var (
	resourceId string
)

func NewRootCmd(log *logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcpproc",
		Short:         "Monitors dcp and cleans up orphaned resources (processes or containers)",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you a development environment
	with minimum remote dependencies and maximum ease of use.

	dcpproc is a monitor tool responsible for cleaning up orphaned resources when DCP exits.`,
		SilenceUsage:     true,
		PersistentPreRun: cmds.LogVersion(log.Logger, "Starting DCPPROC..."),
		Hidden:           true,
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	log.AddLevelFlag(rootCmd.PersistentFlags())

	rootCmd.PersistentFlags().StringVarP(&resourceId, "resource-id", "r", "", "An optional identifier for the resource being monitored. This is used for logging purposes only.")

	var err error
	var cmd *cobra.Command

	if cmd, err = NewProcessCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'monitor-process' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	if cmd, err = NewContainerCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'monitor-container' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	if cmd, err = NewStopProcessTreeCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'stop-process-tree' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	return rootCmd, nil
}
