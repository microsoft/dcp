/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package termpty provides the terminal/PTY primitives used by both the
// executable runner (for executables started under WithTerminal) and the
// container reconciler (for containers started under WithTerminal). It
// abstracts over the platform-specific PTY implementation (ConPTY on
// Windows; not yet implemented on other platforms) and the HMP v1 listener
// that bridges the host PTY to the Aspire terminal host.
package termpty

import (
	"context"
	"errors"

	"github.com/microsoft/dcp/internal/hmp1"
)

// ErrTerminalNotSupported is returned when terminal/PTY support is requested
// on a platform where DCP does not yet implement it. The initial slice
// (Aspire 13.4) supports Windows only via ConPTY; Linux/macOS support
// follows in a subsequent change.
var ErrTerminalNotSupported = errors.New("terminal/PTY support is not implemented on this platform")

// Process represents a process running attached to a pseudo-terminal,
// plus the PTY interface used by the HMP v1 server to bridge it to the
// Aspire terminal host.
type Process struct {
	// PTY is the ReadWriteCloser exposing the PTY master end. Implements
	// hmp1.PTY (Read, Write, Resize, Close).
	PTY hmp1.PTY

	// PID is the OS process id of the spawned process.
	PID int

	// WaitExit blocks until the process exits and returns the exit code.
	// Called at most once.
	WaitExit func() int32
}

// CommandSpec captures everything needed to spawn a process attached to a
// freshly allocated pseudo-terminal. It deliberately mirrors only the inputs
// the underlying ConPTY API needs and is independent of any specific resource
// kind (Executable, Container, ...).
type CommandSpec struct {
	// CommandLine is the full command line in CreateProcessW form (a single
	// string with arguments embedded and quoted as appropriate). Callers can
	// build this via BuildWindowsCommandLine.
	CommandLine string

	// Env is the environment block passed to the child process (each entry
	// is "KEY=VALUE"). May be nil to inherit the parent's environment.
	Env []string

	// WorkingDirectory is the cwd for the child process. Empty inherits the
	// parent's cwd.
	WorkingDirectory string

	// Cols/Rows are the initial dimensions of the allocated PTY. If either
	// is <= 0 the implementation falls back to a sensible default
	// (currently 80x24).
	Cols int
	Rows int
}

// StartProcess allocates a pseudo-terminal and starts a process attached to
// it according to the supplied CommandSpec. The returned Process must be
// closed (via Process.PTY.Close) by the caller when the process exits or the
// terminal is being torn down.
//
// The implementation is platform-specific. See pty_windows.go and
// pty_other.go.
func StartProcess(ctx context.Context, spec CommandSpec) (*Process, error) {
	return startProcessImpl(ctx, spec)
}
