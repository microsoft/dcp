/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"io"

	"github.com/microsoft/dcp/internal/dap"
)

// bridgeOutputHandler routes DAP output events to the appropriate writers
// based on their category. It implements dap.OutputHandler.
type bridgeOutputHandler struct {
	stdout io.Writer
	stderr io.Writer
}

var _ dap.OutputHandler = (*bridgeOutputHandler)(nil)

// newBridgeOutputHandler creates a new bridgeOutputHandler that routes
// "stdout" and "console" output to the stdout writer, and "stderr" output
// to the stderr writer. Either writer may be nil, in which case output
// for that category is silently discarded.
func newBridgeOutputHandler(stdout, stderr io.Writer) *bridgeOutputHandler {
	return &bridgeOutputHandler{
		stdout: stdout,
		stderr: stderr,
	}
}

// HandleOutput routes the output to the appropriate writer based on category.
// "stdout" and "console" categories are written to the stdout writer.
// "stderr" category is written to the stderr writer.
// Other categories are silently discarded.
func (h *bridgeOutputHandler) HandleOutput(category string, output string) {
	switch category {
	case "stdout", "console":
		if h.stdout != nil {
			_, _ = h.stdout.Write([]byte(output))
		}
	case "stderr":
		if h.stderr != nil {
			_, _ = h.stderr.Write([]byte(output))
		}
	}
}
