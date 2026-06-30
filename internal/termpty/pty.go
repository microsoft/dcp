/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package termpty provides the terminal/PTY primitives for working with process terminals.
// It also supports external terminal emulators attaching to processes via HMP v1-compatible
// terminal host implementations.
// For more information on the HMP v1 protocol, see https://github.com/mitchdenny/hex1b/blob/main/docs/muxer-protocol.md.

package termpty

import (
	"context"
	"errors"
	"io"
	"math"
	"os/exec"
	"time"

	"github.com/microsoft/dcp/pkg/process"
)

const (
	defaultInitialCols   = uint16(80)
	defaultInitialRows   = uint16(24)
	maxTerminalDimension = math.MaxInt16
)

var ErrTerminalNotSupported = errors.New("terminal/PTY support is not available on this platform")

// PTY is the abstraction over a pseudo-terminal that the rest of the package
// (PseudoTerminalProcess, Hmp1Server, etc.) consumes. Implementations must be
// safe for concurrent use by separate goroutines for Read, Write and Resize.
type PTY interface {
	io.ReadWriteCloser

	// Resize updates the PTY window size.
	Resize(cols, rows uint16) error
}

// PseudoTerminalProcess represents a running process attached to a pseudo-terminal.
type PseudoTerminalProcess struct {
	// The pseudo-terminal for the process.
	PTY PTY

	// The PID of the process.
	PID process.Pid_t

	// Identity time for identifying the process.
	IdentityTime time.Time

	// StartWaitForExit is a function that starts waiting for the process to exit.
	StartWaitForExit func()

	// ExitHandler captures the exit status of the process and can be used to wait for it.
	// It is allocated by StartProcessWithTerminal and is always non-nil on a successfully started process.
	ExitHandler *process.ConcurrentProcessExitHandler

	// The process executor used to create the process--used to stop the process if needed.
	Executor process.Executor
}

func (ptp *PseudoTerminalProcess) Stop(options ...process.ProcessStopOption) error {
	if ptp.Executor == nil {
		return errors.New("process executor is not available, cannot stop process") // Should never happen
	}
	return ptp.Executor.StopProcess(process.NewHandle(ptp.PID, ptp.IdentityTime), options...)
}

// CommandSpec captures data needed to spawn a command attached to a freshly allocated pseudo-terminal.
type CommandSpec struct {
	// Command to run (including arguments, environment, and working directory).
	Cmd *exec.Cmd

	// Process creation flags.
	CreationFlags process.ProcessCreationFlag

	// Cols/Rows are the initial dimensions of the allocated PTY.
	// If either is == 0 the implementation falls back to a sensible default (currently 80x24).
	Cols uint16
	Rows uint16
}

func NormalizeTerminalDimensions[T int32 | uint16](cols, rows T) (uint16, uint16) {
	var normalizedCols, normalizedRows uint16
	if cols == 0 {
		normalizedCols = defaultInitialCols
	} else if cols > maxTerminalDimension {
		normalizedCols = maxTerminalDimension
	} else {
		normalizedCols = uint16(cols)
	}

	if rows == 0 {
		normalizedRows = defaultInitialRows
	} else if rows > maxTerminalDimension {
		normalizedRows = maxTerminalDimension
	} else {
		normalizedRows = uint16(rows)
	}

	return normalizedCols, normalizedRows
}

// TerminalProcessFactory spawns a process attached to a freshly allocated pseudo-terminal.
// Provided context is used to control the lifecycle of the process and its associated PTY.
type TerminalProcessFactory func(
	ctx context.Context,
	pe process.Executor,
	spec *CommandSpec,
) (*PseudoTerminalProcess, error)

// StartProcessWithTerminal is the standard implementation of TerminalProcessFactory,
// with implementation for Windows, Linux, and MacOS.
func StartProcessWithTerminal(
	ctx context.Context,
	pe process.Executor,
	spec *CommandSpec,
) (*PseudoTerminalProcess, error) {
	// Make sure the command's arguments are consistent with its path.
	if spec != nil && spec.Cmd != nil && spec.Cmd.Path != "" {
		switch {
		case len(spec.Cmd.Args) == 0:
			spec.Cmd.Args = []string{spec.Cmd.Path}
		case spec.Cmd.Args[0] != spec.Cmd.Path:
			spec.Cmd.Args[0] = spec.Cmd.Path
		}
	}
	return startProcessWithTerminal(ctx, pe, spec)
}
