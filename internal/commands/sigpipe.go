/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
)

// handleSigpipeSignalsAnnotation marks commands that handle SIGPIPE instead of being terminated
// when their stdout or stderr pipe is broken.
const handleSigpipeSignalsAnnotation = "dcp.microsoft.com/handle-sigpipe-signals"

// HandleSigpipeSignals marks a command to handle SIGPIPE for the process lifetime, so writes to broken
// stdout or stderr pipes return EPIPE instead of terminating the process.
// Use it for commands that must complete cleanup after their owner (and the reader of their output) exits.
func HandleSigpipeSignals(cmd *cobra.Command) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[handleSigpipeSignalsAnnotation] = "true"
}

// IsSigpipeSignalHandlingEnabled reports whether the command was marked with HandleSigpipeSignals.
func IsSigpipeSignalHandlingEnabled(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Annotations[handleSigpipeSignalsAnnotation] == "true"
}

// PrepareCommandExecution returns a root PersistentPreRun function that applies process-wide settings
// requested by the executing command, then logs program start information.
// Settings are applied before any output is written, so that the startup log line is already covered.
func PrepareCommandExecution(log logr.Logger, programStartMsg string) func(cmd *cobra.Command, args []string) {
	logVersion := LogVersion(log, programStartMsg)
	return func(cmd *cobra.Command, args []string) {
		if IsSigpipeSignalHandlingEnabled(cmd) {
			handleSigpipeSignalsForProcessLifetime()
		}
		logVersion(cmd, args)
	}
}
