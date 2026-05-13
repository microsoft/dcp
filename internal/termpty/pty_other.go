//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"

	"github.com/microsoft/dcp/pkg/process"
)

// unixPtyAdapter wraps the master side of a Unix pseudo-terminal so it
// satisfies hmp1.PTY. The master is an *os.File (typically /dev/ptmx);
// Read/Write target the slave side which is connected to the child's
// stdio. Resize translates cols/rows into a TIOCSWINSZ ioctl via
// pty.Setsize.
type unixPtyAdapter struct {
	master *os.File
}

func (a *unixPtyAdapter) Read(p []byte) (int, error)  { return a.master.Read(p) }
func (a *unixPtyAdapter) Write(p []byte) (int, error) { return a.master.Write(p) }
func (a *unixPtyAdapter) Close() error                { return a.master.Close() }

func (a *unixPtyAdapter) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid PTY dimensions cols=%d rows=%d", cols, rows)
	}
	// pty.Setsize takes (rows, cols) order via the Winsize struct; cols=X, rows=Y
	// matches the kernel's struct winsize { ws_row; ws_col; ... } layout.
	return pty.Setsize(a.master, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// startProcessImpl allocates a Unix pseudo-terminal (/dev/ptmx), spawns the
// command attached to the slave side, and returns a PseudoTerminalProcess
// whose PTY adapter is the master. The caller is responsible for invoking
// PTY.Close() once the session ends; that closes the master fd and the
// kernel SIGHUPs the child.
//
// Linux and macOS share the same Unix98 PTY semantics, so a single
// implementation covers both. Other Unix variants (FreeBSD, NetBSD, etc.)
// are not officially supported, but creack/pty has working implementations
// for them that should function identically.
func startProcessImpl(_ context.Context, spec CommandSpec) (*PseudoTerminalProcess, error) {
	if len(spec.Cmd) == 0 {
		return nil, errors.New("command is empty")
	}
	cols, rows := normalizeDimensions(spec.Cols, spec.Rows)

	// exec.Command sets cmd.lookPathErr internally so cmd.Start() resolves
	// an unqualified executable name (e.g. "dotnet", "npm") against PATH at
	// fork/exec time. A hand-constructed exec.Cmd with Path="dotnet" passes
	// the literal string straight to syscall.Exec and fails with "no such
	// file or directory" — see DCP commit ca7b9d1 for the matching Windows
	// fix.
	cmd := exec.Command(spec.Cmd[0], spec.Cmd[1:]...)
	if env := envMapToSlice(spec.Env); env != nil {
		cmd.Env = env
	}
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}

	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("failed to start process under PTY: %w", err)
	}

	pid, pidErr := process.Int64_ToPidT(int64(cmd.Process.Pid))
	if pidErr != nil {
		// Should never happen on a successfully-started process, but fail
		// loudly if it does so the terminal session never half-initialises.
		_ = master.Close()
		return nil, fmt.Errorf("the started process has invalid PID: %w", pidErr)
	}

	return &PseudoTerminalProcess{
		PTY:          &unixPtyAdapter{master: master},
		PID:          pid,
		IdentityTime: time.Now(),

		WaitExit: func(_ context.Context) int32 {
			// cmd.Wait blocks until the process exits and reaps it. Note
			// the context here is intentionally unused: we cannot actually
			// abort a wait without forcibly killing the child, and the
			// caller already handles cancellation by tearing the session
			// (and therefore the master fd) down separately.
			waitErr := cmd.Wait()
			if waitErr == nil {
				return 0
			}
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				// ExitCode is -1 if the process was signalled; surface the
				// raw value so the terminal host can distinguish "exited
				// abnormally" from "we lost the wait".
				return int32(exitErr.ExitCode())
			}
			// Wait failures other than ExitError mean we never got a real
			// exit status; surface UnknownExitCode-equivalent (-1) per the
			// Windows path's convention.
			return -1
		},
	}, nil
}

// BuildWindowsCommandLine is a no-op stub on non-Windows platforms; it lets
// callers reference it unconditionally without a build tag. Returns the
// empty string so accidental use surfaces obviously.
func BuildWindowsCommandLine(_ string, _ []string) string {
	return ""
}
