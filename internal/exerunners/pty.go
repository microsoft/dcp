/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"errors"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/hmp1"
)

// ErrTerminalNotSupported is returned when terminal/PTY support is requested
// for an Executable on a platform where DCP does not yet implement it. The
// initial slice (Aspire 13.4) supports Windows only via ConPTY; Linux/macOS
// support follows in a subsequent change.
var ErrTerminalNotSupported = errors.New("terminal/PTY support is not implemented on this platform")

// terminalProcess represents a process running attached to a pseudo-terminal,
// plus the PTY interface used by the HMP v1 server to bridge it to the Aspire
// terminal host.
type terminalProcess struct {
	// pty is the ReadWriteCloser exposing the PTY master end. Implements
	// hmp1.PTY (Read, Write, Resize, Close).
	pty hmp1.PTY

	// pid is the OS process id of the spawned process.
	pid int

	// waitExit blocks until the process exits and returns the exit code.
	// Called at most once.
	waitExit func() int32
}

// startTerminalProcess allocates a pseudo-terminal and starts the Executable
// attached to it. The returned terminalProcess must be Closed by the caller
// when the process exits or when the run is being stopped.
//
// The implementation is platform-specific. See pty_windows.go and
// pty_other.go.
func startTerminalProcess(ctx context.Context, exe *apiv1.Executable) (*terminalProcess, error) {
	return startTerminalProcessImpl(ctx, exe)
}
