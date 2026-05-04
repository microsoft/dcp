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
	"os/exec"
	"time"

	"github.com/microsoft/dcp/internal/hmp1"
	"github.com/microsoft/dcp/pkg/process"
)

// PseudoTerminalProcess represents a running process attached to a pseudo-terminal.
// UNDONE: make PseudoTerminalProcess a process.WaitableProcess and delete WaitExit() method.
type PseudoTerminalProcess struct {
	// The pseudo-terminal for the process.
	PTY hmp1.PTY

	// Process ID of the running process.
	PID process.Pid_t

	// Identity time of the running process, for disambiguating the process instance in case the OS re-uses the PID.
	// May not be a valid wall-clock time on platforms that do clock shifting (esp. inside containers).
	IdentityTime time.Time

	WaitExit func(context.Context) int32
}

// CommandSpec captures everything needed to spawn a command attached to a
// freshly allocated pseudo-terminal.
type CommandSpec struct {
	exec.Cmd

	// Cols/Rows are the initial dimensions of the allocated PTY.
	// If either is <= 0 the implementation falls back to a sensible default (currently 80x24).
	Cols int
	Rows int
}

// UNDONE: this probably needs to go to process package.
//
// StartProcess allocates a pseudo-terminal and starts a process attached to
// it according to the supplied CommandSpec.
//
// The returned Process must be
// closed (via Process.PTY.Close) by the caller when the process exits or the
// terminal is being torn down.
// UNDONE: can we make it so this is not a requirement and happens automatically when the process exits?
func StartProcess(ctx context.Context, spec CommandSpec) (*PseudoTerminalProcess, error) {
	return startProcessImpl(ctx, spec)
}
