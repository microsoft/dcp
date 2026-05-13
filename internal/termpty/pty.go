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
	"time"

	"github.com/microsoft/dcp/internal/hmp1"
	"github.com/microsoft/dcp/pkg/process"
)

// ErrTerminalNotSupported is returned by StartProcess on platforms where the
// PTY/terminal integration is not yet implemented (currently any non-Windows
// host). Callers should treat this as a feature-not-available signal rather
// than a hard error and fall back to non-terminal process execution.
var ErrTerminalNotSupported = errors.New("terminal/PTY support is not available on this platform")

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
// freshly allocated pseudo-terminal. The shape is deliberately
// process-API-agnostic so the same struct works for ConPTY (Windows) and
// /dev/ptmx (Unix) without leaking exec.Cmd's wider surface (Stdin/Stdout/etc
// are not meaningful for a PTY-attached child).
type CommandSpec struct {
	// Cmd is the command line as an argv-style slice. The first element is
	// the executable path or name; remaining elements are arguments. If the
	// first element does not contain a path separator the implementation
	// resolves it against PATH. Must be non-empty.
	Cmd []string

	// Env is the environment for the child process, keyed by variable name.
	// If nil, the child inherits the parent's environment.
	Env map[string]string

	// Dir is the working directory for the child process. Empty inherits
	// the parent's cwd.
	Dir string

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

// envMapToSlice flattens an env map into the "KEY=VALUE" slice form expected
// by exec.Cmd.Env / conpty.ConPtyEnv. Returns nil for a nil input so callers
// can distinguish "inherit parent env" from "empty env".
func envMapToSlice(env map[string]string) []string {
	if env == nil {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
