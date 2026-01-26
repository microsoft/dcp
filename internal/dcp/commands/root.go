/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
	ctrlruntime "sigs.k8s.io/controller-runtime"

	cmds "github.com/microsoft/dcp/internal/commands"
	dcpctrl_cmds "github.com/microsoft/dcp/internal/dcpctrl/commands"
	dcpproc_cmds "github.com/microsoft/dcp/internal/dcpproc/commands"
	dcptun_cmds "github.com/microsoft/dcp/internal/dcptun/commands"
	"github.com/microsoft/dcp/pkg/logger"
)

func NewRootCmd(log *logger.Logger) (*cobra.Command, error) {
	rootCmd := &cobra.Command{
		SilenceErrors: true,
		Use:           "dcp",
		Short:         "Runs and manages multi-service applications and their dependencies",
		Long: `DCP is a developer tool for running multi-service applications.

	It integrates your code, emulators and containers to give you a development environment
	with minimum remote dependencies and maximum ease of use.`,
		SilenceUsage:     true,
		PersistentPreRun: cmds.LogVersion(log.Logger, "Starting DCP..."),
	}

	rootCmd.CompletionOptions.HiddenDefaultCmd = true

	log.AddLevelFlag(rootCmd.PersistentFlags())

	var err error
	var cmd *cobra.Command

	if cmd, err = cmds.NewVersionCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'version' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	if cmd, err = NewInfoCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'info' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	if cmd, err = NewStartApiSrvCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'start-apiserver' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	if cmd, err = NewSessionLogCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'session-log' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	// Add dcpctrl sub-commands
	rootCmd.AddCommand(dcpctrl_cmds.NewRunControllersCommand(log.Logger))

	// Add dcpproc sub-commands
	if cmd, err = dcpproc_cmds.NewProcessCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'monitor-process' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	if cmd, err = dcpproc_cmds.NewContainerCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'monitor-container' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	if cmd, err = dcpproc_cmds.NewStopProcessTreeCommand(log.Logger); err != nil {
		return nil, fmt.Errorf("could not set up 'stop-process-tree' command: %w", err)
	} else {
		rootCmd.AddCommand(cmd)
	}

	// Add dcptun sub-commands
	rootCmd.AddCommand(dcptun_cmds.NewRunServerCommand(log.Logger))

	ctrlruntime.SetLogger(log.Logger.V(1))
	klog.SetLogger(log.Logger.V(1))

	return rootCmd, nil
}
