/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Shared test helpers (common to all platforms).

package termpty_test

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/termpty"
	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	defaultTestTimeout = 20 * time.Second

	// drainExitTimeout bounds how long the per-test cleanup waits for the
	// process exit notification after the lifetime context has been
	// cancelled. The OS executor escalates graceful-stop -> force-kill well
	// within this window for any well-behaved (or even misbehaved) child.
	drainExitTimeout = 10 * time.Second
)

// startTermchildWithPTY spawns the termchild binary with the supplied
// argument list attached to a freshly allocated PTY using a shared
// OSExecutor whose lifetime is bound to t.Cleanup. The passed ctx controls
// the process lifetime.
func startTermchildWithPTY(t *testing.T, ctx context.Context, args ...string) *termpty.PseudoTerminalProcess {
	t.Helper()

	log := testutil.NewLogForTesting(t.Name())
	executor := process.NewOSExecutor(log)
	t.Cleanup(executor.Dispose)

	termChildPath, err := int_testutil.GetTestToolPath("termchild")
	require.NoError(t, err, "could not locate termchild test tool (did you run `make test-prereqs`?)")

	spec := &termpty.CommandSpec{
		Cmd: exec.Command(termChildPath, args...),
	}

	ptp, err := termpty.StartProcessWithTerminal(ctx, executor, spec)
	require.NoError(t, err, "StartProcessWithTerminal failed")
	require.NotNil(t, ptp)
	require.NotNil(t, ptp.ExitHandler, "StartProcessWithTerminal must allocate an ExitHandler")

	ptp.StartWaitForExit()

	t.Cleanup(func() {
		// Close PTY first so the child receives a HUP-equivalent if it is
		// still running. This also unblocks any reader still sitting in
		// master.Read.
		ptyCloseErr := ptp.PTY.Close()
		require.NoError(t, ptyCloseErr, "failed to close PTY")

		// Wait for the exit notification so the executor does not hang on
		// delivering it.
		select {
		case <-ptp.ExitHandler.Exited():
		case <-time.After(drainExitTimeout):
			t.Errorf("timed out waiting for process exit notification during cleanup")
		}
	})

	return ptp
}

// readUntil reads from r until target is observed anywhere in the
// accumulated output, ctx is done, or a read error occurs.
// Returns the accumulated output and any terminal error.
func readUntil(ctx context.Context, r io.Reader, target string) (string, error) {
	type readResult struct {
		data []byte
		err  error
	}

	var accumulated bytes.Buffer
	for {
		if strings.Contains(accumulated.String(), target) {
			return accumulated.String(), nil
		}

		buf := make([]byte, 4096)
		resCh := make(chan readResult, 1)
		go func() {
			n, err := r.Read(buf)
			if n > 0 {
				resCh <- readResult{data: buf[:n], err: err}
			} else {
				resCh <- readResult{data: nil, err: err}
			}
		}()

		select {
		case <-ctx.Done():
			return accumulated.String(), ctx.Err()
		case res := <-resCh:
			if len(res.data) > 0 {
				accumulated.Write(res.data)
			}
			if res.err != nil {
				if strings.Contains(accumulated.String(), target) {
					return accumulated.String(), nil
				}
				return accumulated.String(), res.err
			}
		}
	}
}

// awaitExit blocks until the exit notification arrives or ctx is done.
func awaitExit(t *testing.T, ctx context.Context, exitHandler *process.ConcurrentProcessExitHandler) process.ProcessExitInfo {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("timed out waiting for process exit: %v", ctx.Err())
		return process.ProcessExitInfo{}
	case <-exitHandler.Exited():
		return exitHandler.ExitInfo()
	}
}
