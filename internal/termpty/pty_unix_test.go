//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// End-to-end tests for StartProcessWithTerminal on Unix. They spawn the
// portable test/termchild helper attached to a PTY rather than running shell
// scripts, so the assertions are deterministic across Linux and macOS hosts
// without requiring bash or specific shell features.
//
// Scenarios that need to assert on external termination supply --wait so the
// child blocks until the test cancels its lifetime context; that ensures any
// "process exited on its own" outcome would surface as a test timeout rather
// than a false positive.

package termpty_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/termpty"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

// TestStartProcessWithTerminal_StdoutFromChild verifies that bytes written to
// the child's stdout flow to the parent via the PTY master.
func TestStartProcessWithTerminal_StdoutFromChild(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	sp := startTermchildWithPTY(t, ctx, "--print", "hello-from-child")

	out, err := readUntil(ctx, sp.PTY, "hello-from-child")
	require.NoError(t, err, "expected to see 'hello-from-child'; got: %q", out)

	ei := awaitExit(t, ctx, sp.ExitHandler)
	require.NoError(t, ei.Err)
	require.Equal(t, int32(0), ei.ExitCode)
}

// TestStartProcessWithTerminal_StderrFromChild verifies that the child's
// stderr is multiplexed onto the same PTY master as stdout.
func TestStartProcessWithTerminal_StderrFromChild(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	sp := startTermchildWithPTY(t, ctx, "--print-stderr", "boom-from-stderr")

	out, err := readUntil(ctx, sp.PTY, "boom-from-stderr")
	require.NoError(t, err, "expected to see 'boom-from-stderr'; got: %q", out)

	ei := awaitExit(t, ctx, sp.ExitHandler)
	require.NoError(t, ei.Err)
	require.Equal(t, int32(0), ei.ExitCode)
}

// TestStartProcessWithTerminal_StdinToChild verifies that bytes written to
// the PTY master are delivered to the child's stdin. The child disables
// terminal echo so the input does not pollute the output stream. The READY
// marker provides synchronization: the test waits for echo to be disabled
// before sending input.
func TestStartProcessWithTerminal_StdinToChild(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	sp := startTermchildWithPTY(t, ctx,
		"--disable-echo",
		"--print", "READY",
		"--echo",
	)

	out, err := readUntil(ctx, sp.PTY, "READY")
	require.NoError(t, err, "expected READY marker; got: %q", out)

	_, err = sp.PTY.Write([]byte("ping\n"))
	require.NoError(t, err)

	out, err = readUntil(ctx, sp.PTY, "got:ping")
	require.NoError(t, err, "expected 'got:ping' echoed back; got: %q", out)

	// Close PTY to deliver EOF to the echo loop and let termchild exit
	// gracefully. The cleanup helper also closes PTY but does so after the
	// test returns, which would defer exit observation past this assertion.
	require.NoError(t, sp.PTY.Close())

	ei := awaitExit(t, ctx, sp.ExitHandler)
	require.NoError(t, ei.Err)
	require.Equal(t, int32(0), ei.ExitCode)
}

// TestStartProcessWithTerminal_AbnormalExitCode verifies that a non-zero
// exit code from the child is propagated through the exit handler.
func TestStartProcessWithTerminal_AbnormalExitCode(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	sp := startTermchildWithPTY(t, ctx, "--exit-code", "42")

	ei := awaitExit(t, ctx, sp.ExitHandler)
	require.NoError(t, ei.Err)
	require.Equal(t, int32(42), ei.ExitCode)
}

// TestStartProcessWithTerminal_ContextCancellationStopsProcess verifies that
// cancelling the lifetime context terminates a running child and produces an
// exit notification. The child blocks in --wait so the only path to exit is
// signal delivery from the executor.
func TestStartProcessWithTerminal_ContextCancellationStopsProcess(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer testCancel()

	procCtx, procCancel := context.WithCancel(testCtx)
	defer procCancel()

	sp := startTermchildWithPTY(t, procCtx, "--print", "READY", "--wait")

	out, err := readUntil(testCtx, sp.PTY, "READY")
	require.NoError(t, err, "expected READY marker; got: %q", out)

	procCancel()

	ei := awaitExit(t, testCtx, sp.ExitHandler)
	// SIGTERM (or SIGKILL escalation) leaves the process without a normal
	// exit code; Go's os/exec reports ExitCode() == -1 for signaled
	// processes.
	require.Equal(t, process.UnknownExitCode, ei.ExitCode,
		"expected UnknownExitCode for signal-terminated process, got %d", ei.ExitCode)
}

// TestStartProcessWithTerminal_SigtermIgnoredEscalatesToKill verifies that
// when the child ignores SIGTERM, the executor escalates to SIGKILL within a
// bounded time and still produces an exit notification.
func TestStartProcessWithTerminal_SigtermIgnoredEscalatesToKill(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer testCancel()

	procCtx, procCancel := context.WithCancel(testCtx)
	defer procCancel()

	sp := startTermchildWithPTY(t, procCtx,
		"--ignore-sigterm",
		"--print", "READY",
		"--wait",
	)

	out, err := readUntil(testCtx, sp.PTY, "READY")
	require.NoError(t, err, "expected READY marker; got: %q", out)

	procCancel()

	ei := awaitExit(t, testCtx, sp.ExitHandler)
	require.Equal(t, process.UnknownExitCode, ei.ExitCode,
		"expected UnknownExitCode for SIGKILL'd process, got %d", ei.ExitCode)
}

// TestStartProcessWithTerminal_PTYCloseDeliversSIGHUP verifies that closing
// the PTY master sends SIGHUP to the child's session. The child's --hup-exit-code
// handler captures it and exits with the chosen code (7) which the exit
// notification surfaces.
func TestStartProcessWithTerminal_PTYCloseDeliversSIGHUP(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	sp := startTermchildWithPTY(t, ctx,
		"--hup-exit-code", "7",
		"--print", "READY",
		"--wait",
	)

	out, err := readUntil(ctx, sp.PTY, "READY")
	require.NoError(t, err, "expected READY marker; got: %q", out)

	require.NoError(t, sp.PTY.Close())

	ei := awaitExit(t, ctx, sp.ExitHandler)
	require.NoError(t, ei.Err)
	require.Equal(t, int32(7), ei.ExitCode,
		"expected exit code 7 from HUP handler, got %d", ei.ExitCode)
}

// TestStartProcessWithTerminal_NormalExitWithPTYStillOpen verifies that a
// child that exits normally while the PTY remains open is reaped, its exit
// code is captured, and subsequent reads from the master return EOF (or the
// platform-specific EIO equivalent) once the child's slave fd is gone.
func TestStartProcessWithTerminal_NormalExitWithPTYStillOpen(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	sp := startTermchildWithPTY(t, ctx, "--print", "bye")

	out, err := readUntil(ctx, sp.PTY, "bye")
	require.NoError(t, err, "expected 'bye' before exit; got: %q", out)

	ei := awaitExit(t, ctx, sp.ExitHandler)
	require.NoError(t, ei.Err)
	require.Equal(t, int32(0), ei.ExitCode)

	// After the child exits, draining the PTY should eventually surface an
	// error: io.EOF on macOS, syscall.EIO on Linux (often wrapped in
	// *os.PathError). The test tolerates either; what matters is that the
	// read does not block forever.
	_, drainErr := readUntil(ctx, sp.PTY, "this-marker-will-never-appear")
	require.Error(t, drainErr, "expected an error from PTY read after child exit")
	require.False(t, errors.Is(drainErr, context.DeadlineExceeded),
		"PTY read should fail with EOF/EIO after child exit, not hit the test deadline")
	require.False(t, errors.Is(drainErr, context.Canceled),
		"PTY read should fail with EOF/EIO after child exit, not via ctx cancel")
}

// TestStartProcessWithTerminal_ResizeObservedByChild verifies that calling
// Resize on the PTY delivers SIGWINCH to the child's session and that the
// new dimensions are visible to the child. The child's --resize-exit-code
// handler prints "size:COLSxROWS" then exits with the chosen code.
func TestStartProcessWithTerminal_ResizeObservedByChild(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	sp := startTermchildWithPTY(t, ctx,
		"--resize-exit-code", "0",
		"--print", "READY",
		"--wait",
	)

	out, err := readUntil(ctx, sp.PTY, "READY")
	require.NoError(t, err, "expected READY marker; got: %q", out)

	const (
		newCols uint16 = 137
		newRows uint16 = 53
	)
	require.NoError(t, sp.PTY.Resize(newCols, newRows))

	expected := "size:137x53"
	out, err = readUntil(ctx, sp.PTY, expected)
	require.NoError(t, err, "expected %q from resize handler; got: %q", expected, out)

	ei := awaitExit(t, ctx, sp.ExitHandler)
	require.NoError(t, ei.Err)
	require.Equal(t, int32(0), ei.ExitCode)
}

// TestStartProcessWithTerminal_FailsForNonexistentBinary verifies that when
// the child cannot be spawned (e.g. binary does not exist),
// StartProcessWithTerminal returns an error and cleans up the allocated PTY.
func TestStartProcessWithTerminal_FailsForNonexistentBinary(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)

	spec := &termpty.CommandSpec{
		Cmd: exec.Command("/this/path/definitely/does/not/exist/dcp-pty-test"),
	}

	ptp, err := termpty.StartProcessWithTerminal(ctx, executor, spec)
	require.Error(t, err)
	require.Nil(t, ptp)
	require.Contains(t, err.Error(), "failed to start process")
}
