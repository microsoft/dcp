//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// End-to-end tests for StartProcessWithTerminal on Windows (ConPTY). They
// spawn the portable test/termchild helper attached to a ConPTY rather than
// running cmd.exe / PowerShell scripts, so the assertions are deterministic
// across Win10 / Win11 / Windows Server hosts without depending on a
// specific shell.
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
// stderr is multiplexed onto the same PTY master as stdout. (ConPTY merges
// stdout and stderr into a single pseudo-console output stream.)
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
// terminal echo and line input mode (ConPTY enables both by default), so the
// input does not pollute the output stream. The READY marker provides
// synchronization: the test waits for echo to be disabled before sending
// input.
//
// The line terminator sent to the PTY is CR+LF. ConPTY's input stream
// follows console conventions where Enter is CR; bufio.Reader on the child
// side splits on LF, so the combination guarantees both behaviors are
// satisfied.
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

	_, err = sp.PTY.Write([]byte("ping\r\n"))
	require.NoError(t, err)

	out, err = readUntil(ctx, sp.PTY, "got:ping")
	require.NoError(t, err, "expected 'got:ping' echoed back; got: %q", out)

	// Close PTY so the child sees EOF on stdin and the echo loop returns.
	// (We also rely on the per-test cleanup to close PTY a second time,
	// which must be a no-op.)
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
// signal delivery (or forced termination) initiated by the executor.
//
// Windows-specific behavior: OSExecutor first attempts a graceful shutdown by
// calling GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, processGroupID). That
// API only signals processes that share the SAME console as the caller. The
// test process runs in its own console (or none at all), while the ConPTY
// child runs attached to a separate conhost.exe — they do not share a
// console, so the CTRL_BREAK signal never reaches the child. The executor's
// graceful-stop timeout therefore elapses and it falls back to
// os.Process.Kill, which on Windows is TerminateProcess(handle, 1) and
// yields exit code 1.
//
// In other words, on Windows this scenario currently exercises the executor
// escalation path more than the graceful shutdown path.
// Routing signals through the pseudo-console (e.g. writing 0x03 to the PTY input) would be
// a public-API change to either internal/termpty or pkg/process,
// and might be considered for future DCP release.
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
	require.Equal(t, int32(1), ei.ExitCode,
		"expected exit code 1 from TerminateProcess fallback (CTRL_BREAK does not cross consoles), got %d", ei.ExitCode)
}

// TestStartProcessWithTerminal_BreakIgnoredEscalatesToKill verifies that when
// the child ignores CTRL_BREAK_EVENT, the executor escalates to forced
// termination (proc.Kill -> TerminateProcess) within a bounded time and
// still produces an exit notification. TerminateProcess uses exit code 1.
func TestStartProcessWithTerminal_BreakIgnoredEscalatesToKill(t *testing.T) {
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
	// Go's os.Process.Kill on Windows calls TerminateProcess(handle, 1),
	// which yields exit code 1.
	require.Equal(t, int32(1), ei.ExitCode,
		"expected exit code 1 from TerminateProcess after CTRL_BREAK was ignored, got %d", ei.ExitCode)
}

// TestStartProcessWithTerminal_PTYCloseDeliversCloseEvent verifies that
// closing the PTY (ClosePseudoConsole) delivers a console close event to the
// child, which termchild surfaces as syscall.SIGTERM. The child's
// --hup-exit-code handler captures it and exits with the chosen code (7).
//
// Per Microsoft's documentation, ClosePseudoConsole disconnects the attached
// process the same way closing a console window does, which delivers
// CTRL_CLOSE_EVENT. The Go runtime exposes that as syscall.SIGTERM (same as
// CTRL_BREAK_EVENT), so termchild's signal dispatcher cannot distinguish the
// two — both fall under its signalHup classification on Windows. When
// --hup-exit-code is configured, the dispatcher uses the override.
func TestStartProcessWithTerminal_PTYCloseDeliversCloseEvent(t *testing.T) {
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
		"expected exit code 7 from CTRL_CLOSE handler, got %d", ei.ExitCode)
}

// TestStartProcessWithTerminal_NormalExitWithPTYStillOpen verifies that a
// child that exits normally while the PTY remains open is reaped, its exit
// code is captured, and that reads on the PTY master surface an error after
// the master is explicitly closed (rather than blocking forever).
//
// Windows-specific behavior: unlike Unix, where the master end of the PTY
// returns EOF/EIO once the slave side is closed (which the kernel does on
// child exit), ConPTY's output pipe is owned by conhost.exe and only closes
// when the host calls ClosePseudoConsole. The child's exit alone is not
// sufficient to unblock a pending read on the output pipe; the test
// therefore closes the PTY before asserting that subsequent reads fail.
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

	// On Windows the ConPTY output pipe stays open until the host closes
	// it; closing the PTY here releases conhost's hold on the write end.
	require.NoError(t, sp.PTY.Close())

	// Drain any residual bytes and then assert the read terminates with an
	// error (typically os.ErrClosed surfaced by windowsPTY.Read after Close,
	// or a broken-pipe error surfaced by ReadFile) rather than blocking
	// until the test deadline.
	_, drainErr := readUntil(ctx, sp.PTY, "this-marker-will-never-appear")
	require.Error(t, drainErr, "expected an error from PTY read after Close")
	require.False(t, errors.Is(drainErr, context.DeadlineExceeded),
		"PTY read should fail with a pipe/closed-handle error after Close, not hit the test deadline")
	require.False(t, errors.Is(drainErr, context.Canceled),
		"PTY read should fail with a pipe/closed-handle error after Close, not via ctx cancel")
}

// TestStartProcessWithTerminal_ResizeObservedByChild verifies that calling
// Resize on the PTY delivers a WINDOW_BUFFER_SIZE_EVENT to the child's
// CONIN$ and that the new dimensions are visible to the child. termchild's
// --resize-exit-code handler prints "size:COLSxROWS" then exits with the
// chosen code.
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
// StartProcessWithTerminal returns an error and cleans up the allocated PTY
// (pseudo-console and pipe handles).
func TestStartProcessWithTerminal_FailsForNonexistentBinary(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)

	spec := &termpty.CommandSpec{
		Cmd: exec.Command(`C:\this\path\definitely\does\not\exist\dcp-pty-test.exe`),
	}

	ptp, err := termpty.StartProcessWithTerminal(ctx, executor, spec)
	require.Error(t, err)
	require.Nil(t, ptp)
	require.Contains(t, err.Error(), "failed to start process")
}
