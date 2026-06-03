//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// White-box tests for windowsPTY and startProcessWithTerminal on Windows.
// These tests need access to internal types (windowsPTY) and helpers; for
// behaviors that can only be observed via the child process (e.g.
// dimensions inside the ConPTY) they spawn the portable test/termchild
// helper attached to a ConPTY and assert on its output.
package termpty

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const internalTestTimeout = 20 * time.Second

// newOpenedWindowsPTY allocates a fresh ConPTY (pseudo-console + I/O pipes)
// and registers a t.Cleanup that closes it. No child process is spawned;
// the test only needs the PTY object itself.
func newOpenedWindowsPTY(t *testing.T) *windowsPTY {
	t.Helper()

	var inputRead, inputWrite, outputRead, outputWrite windows.Handle
	require.NoError(t, windows.CreatePipe(&inputRead, &inputWrite, nil, 0))
	require.NoError(t, windows.CreatePipe(&outputRead, &outputWrite, nil, 0))

	var hConsole windows.Handle
	err := windows.CreatePseudoConsole(
		windowsConsoleSize(0, 0),
		inputRead, outputWrite, 0, &hConsole,
	)
	require.NoError(t, err, "CreatePseudoConsole failed")

	p := &windowsPTY{
		hConsole:     hConsole,
		outputRead:   outputRead,
		inputWrite:   inputWrite,
		otherHandles: []windows.Handle{inputRead, outputWrite},
		lock:         &sync.Mutex{},
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestWindowsPTY_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	pty := newOpenedWindowsPTY(t)

	require.NoError(t, pty.Close())
	require.Equal(t, windows.InvalidHandle, pty.hConsole)
	require.Equal(t, windows.InvalidHandle, pty.outputRead)
	require.Equal(t, windows.InvalidHandle, pty.inputWrite)
	require.Nil(t, pty.otherHandles, "otherHandles slice should be cleared after Close")

	// Second call should not return an error.
	require.NoError(t, pty.Close())
}

func TestWindowsPTY_CloseOnZeroValueIsNoop(t *testing.T) {
	t.Parallel()
	pty := &windowsPTY{
		lock: &sync.Mutex{},
	}

	require.NoError(t, pty.Close())
	// Run it again to confirm idempotency on the zero value.
	require.NoError(t, pty.Close())
}

func TestWindowsPTY_ReadAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	pty := newOpenedWindowsPTY(t)
	require.NoError(t, pty.Close())

	n, err := pty.Read(make([]byte, 16))
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestWindowsPTY_WriteAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	pty := newOpenedWindowsPTY(t)
	require.NoError(t, pty.Close())

	n, err := pty.Write([]byte("hello"))
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestWindowsPTY_ResizeAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	pty := newOpenedWindowsPTY(t)
	require.NoError(t, pty.Close())

	require.ErrorIs(t, pty.Resize(80, 24), os.ErrClosed)
}

func TestWindowsPTY_ResizeRejectsZeroDimensions(t *testing.T) {
	t.Parallel()
	pty := newOpenedWindowsPTY(t)

	cases := []struct {
		name       string
		cols, rows uint16
	}{
		{"zero cols", 0, 24},
		{"zero rows", 80, 0},
		{"both zero", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pty.Resize(c.cols, c.rows)
			require.Error(t, err)
			require.Contains(t, err.Error(), "invalid PTY dimensions")
		})
	}
}

// TestWindowsPTY_ResizeRoundTrips drives Resize and confirms the child
// observes the updated dimensions via termchild --report-size. ConPTY does
// not expose a host-side "get size" API, so we observe through the child.
func TestWindowsPTY_ResizeRoundTrips(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)

	// Start with non-default dimensions; resize once; then verify by
	// spawning a fresh termchild that prints init-size.
	const (
		initialCols uint16 = 100
		initialRows uint16 = 30
		resizedCols uint16 = 175
		resizedRows uint16 = 47
	)

	termchildPath, err := int_testutil.GetTestToolPath("termchild")
	require.NoError(t, err, "could not locate termchild test tool (did you run `make test-prereqs`?)")

	spec := &CommandSpec{
		Cmd: exec.Command(termchildPath,
			"--report-size",
			"--resize-exit-code", "0",
			"--wait",
			"--wait-max-duration", "30s",
		),
		Cols: initialCols,
		Rows: initialRows,
	}

	ptp, err := StartProcessWithTerminal(ctx, executor, spec)
	require.NoError(t, err)
	require.NotNil(t, ptp)
	t.Cleanup(func() { _ = ptp.PTY.Close() })

	wp, ok := ptp.PTY.(*windowsPTY)
	require.True(t, ok, "expected *windowsPTY, got %T", ptp.PTY)

	// First, confirm the initial dimensions came through.
	out, err := readUntilString(ctx, t, ptp.PTY, "init-size:")
	require.NoError(t, err)
	require.Contains(t, out, "init-size:100x30",
		"expected initial size 100x30 in child output, got %q", out)

	// Now resize and wait for the resize handler to print the new size.
	require.NoError(t, wp.Resize(resizedCols, resizedRows))

	out, err = readUntilString(ctx, t, ptp.PTY, "size:175x47")
	require.NoError(t, err, "expected resized size in child output, got %q", out)
}

// TestStartProcessWithTerminal_DefaultDimensionsApplied verifies that a zero
// Cols/Rows spec causes the default 80x24 to be applied to the allocated
// ConPTY (observed via the child's init-size report).
func TestStartProcessWithTerminal_DefaultDimensionsApplied(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)

	termchildPath, err := int_testutil.GetTestToolPath("termchild")
	require.NoError(t, err, "could not locate termchild test tool (did you run `make test-prereqs`?)")

	spec := &CommandSpec{
		Cmd: exec.Command(termchildPath,
			"--report-size",
			"--wait",
			"--wait-max-duration", "30s",
		),
	}

	ptp, err := StartProcessWithTerminal(ctx, executor, spec)
	require.NoError(t, err)
	require.NotNil(t, ptp)
	t.Cleanup(func() { _ = ptp.PTY.Close() })

	_, ok := ptp.PTY.(*windowsPTY)
	require.True(t, ok, "expected *windowsPTY, got %T", ptp.PTY)

	expected := "init-size:" + uintToString(defaultInitialCols) + "x" + uintToString(defaultInitialRows)
	out, err := readUntilString(ctx, t, ptp.PTY, expected)
	require.NoError(t, err, "expected %q in child output, got %q", expected, out)
}

// TestStartProcessWithTerminal_CustomDimensionsApplied verifies that a
// non-zero Cols/Rows spec is propagated to the ConPTY (observed via the
// child's init-size report).
func TestStartProcessWithTerminal_CustomDimensionsApplied(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, internalTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting(t.Name())
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)

	const (
		wantCols uint16 = 132
		wantRows uint16 = 50
	)

	termchildPath, err := int_testutil.GetTestToolPath("termchild")
	require.NoError(t, err, "could not locate termchild test tool (did you run `make test-prereqs`?)")

	spec := &CommandSpec{
		Cmd: exec.Command(termchildPath,
			"--report-size",
			"--wait",
			"--wait-max-duration", "30s",
		),
		Cols: wantCols,
		Rows: wantRows,
	}

	ptp, err := StartProcessWithTerminal(ctx, executor, spec)
	require.NoError(t, err)
	require.NotNil(t, ptp)
	t.Cleanup(func() { _ = ptp.PTY.Close() })

	_, ok := ptp.PTY.(*windowsPTY)
	require.True(t, ok)

	expected := "init-size:" + uintToString(wantCols) + "x" + uintToString(wantRows)
	out, err := readUntilString(ctx, t, ptp.PTY, expected)
	require.NoError(t, err, "expected %q in child output, got %q", expected, out)
}

// readUntilString accumulates bytes from r until target appears in the
// accumulated buffer or ctx is done. Intended for ConPTY scenarios where
// the read returns an error after the child writes (ConPTY can occasionally
// return ERROR_BROKEN_PIPE between writes if the pty is being torn down);
// callers want to inspect the accumulated text and decide.
func readUntilString(ctx context.Context, t *testing.T, r interface {
	Read([]byte) (int, error)
}, target string) (string, error) {
	t.Helper()
	type readResult struct {
		n   int
		err error
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		if strings.Contains(sb.String(), target) {
			return sb.String(), nil
		}
		resCh := make(chan readResult, 1)
		go func() {
			n, err := r.Read(buf)
			resCh <- readResult{n: n, err: err}
		}()
		select {
		case <-ctx.Done():
			return sb.String(), ctx.Err()
		case res := <-resCh:
			if res.n > 0 {
				sb.Write(buf[:res.n])
			}
			if res.err != nil {
				if strings.Contains(sb.String(), target) {
					return sb.String(), nil
				}
				return sb.String(), res.err
			}
		}
	}
}

func uintToString(v uint16) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	var buf [6]byte
	n := len(buf)
	for v > 0 {
		n--
		buf[n] = digits[v%10]
		v /= 10
	}
	return string(buf[n:])
}
