//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// White-box tests for unixPTY and startProcessWithTerminal that need access to
// internal types (unixPTY) and helpers (pty_open, pty_get_size, etc.).
package termpty

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const internalTestTimeout = 20 * time.Second

// newOpenedUnixPTY allocates a fresh unixPTY and registers a t.Cleanup that
// closes it. The slave side stays open in the test process (no child is
// spawned) so tests can drive the slave end directly.
func newOpenedUnixPTY(t *testing.T) *unixPTY {
	t.Helper()
	master, slave, err := pty_open()
	require.NoError(t, err, "pty_open failed")
	p := &unixPTY{master: master, slave: slave}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestUnixPTY_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)

	require.NoError(t, pty.Close())
	require.Nil(t, pty.master, "master should be nil after first Close")
	require.Nil(t, pty.slave, "slave should be nil after first Close")

	// Second call should not return an error.
	require.NoError(t, pty.Close())
}

func TestUnixPTY_CloseAfterSlaveAlreadyClosed(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)

	require.NoError(t, pty.closeSlave())
	require.Nil(t, pty.slave)
	require.NotNil(t, pty.master, "master should still be open")

	require.NoError(t, pty.Close())
	require.Nil(t, pty.master)
}

func TestUnixPTY_ReadAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)
	require.NoError(t, pty.Close())

	n, err := pty.Read(make([]byte, 16))
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestUnixPTY_WriteAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)
	require.NoError(t, pty.Close())

	n, err := pty.Write([]byte("hello"))
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestUnixPTY_ResizeAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)
	require.NoError(t, pty.Close())

	require.ErrorIs(t, pty.Resize(80, 24), os.ErrClosed)
}

func TestUnixPTY_ResizeRejectsZeroDimensions(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)

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

func TestUnixPTY_ResizeRoundTrips(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)

	require.NoError(t, pty.Resize(120, 40))
	ws, err := pty_get_size(pty.master)
	require.NoError(t, err)
	require.Equal(t, uint16(120), ws.cols)
	require.Equal(t, uint16(40), ws.rows)

	require.NoError(t, pty.Resize(200, 60))
	ws, err = pty_get_size(pty.master)
	require.NoError(t, err)
	require.Equal(t, uint16(200), ws.cols)
	require.Equal(t, uint16(60), ws.rows)
}

// TestUnixPTY_ReadReceivesSlaveOutput exercises the slave -> master direction
// (which is what unixPTY.Read does in production: pulls output produced by the
// child process). Output processing on the slave side translates only NL to
// CRLF (ONLCR); our payload uses ASCII characters with no special handling,
// so it survives the line discipline unchanged.
func TestUnixPTY_ReadReceivesSlaveOutput(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)

	// Bound the read so a failure to forward the data doesn't hang the test.
	deadline := time.Now().Add(internalTestTimeout)
	require.NoError(t, pty.master.SetReadDeadline(deadline))

	const payload = "slave-data-abcdef"
	n, err := pty.slave.Write([]byte(payload))
	require.NoError(t, err)
	require.Equal(t, len(payload), n)

	buf := make([]byte, 256)
	rn, err := pty.Read(buf)
	require.NoError(t, err)
	require.Greater(t, rn, 0)
	require.Contains(t, string(buf[:rn]), payload)
}

// TestUnixPTY_WriteReachesSlave exercises the master -> slave direction
// (which is what unixPTY.Write does in production: delivers client keystrokes
// to the child). The slave is in canonical mode by default, so the write
// includes a newline to terminate the line so slave.Read returns immediately.
func TestUnixPTY_WriteReachesSlave(t *testing.T) {
	t.Parallel()
	pty := newOpenedUnixPTY(t)

	deadline := time.Now().Add(internalTestTimeout)
	require.NoError(t, pty.slave.SetReadDeadline(deadline))

	const payload = "master-data-xyz\n"
	n, err := pty.Write([]byte(payload))
	require.NoError(t, err)
	require.Equal(t, len(payload), n)

	// Echo of the input may also flow back to master; we don't read master
	// here so we don't care about that path. We only care that the slave
	// receives the data.
	buf := make([]byte, 256)
	rn, err := pty.slave.Read(buf)
	require.NoError(t, err)
	require.Greater(t, rn, 0)
	require.True(t, strings.Contains(string(buf[:rn]), "master-data-xyz"),
		"slave should receive the bytes written to master; got %q", string(buf[:rn]))
}

// TestStartProcessWithTerminal_DefaultDimensionsApplied verifies that a zero
// Cols/Rows spec causes the default 80x24 to be applied to the allocated PTY.
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
		// --wait so the process is still running when the test inspects PTY
		// dimensions; --wait-max-duration ensures a stuck test doesn't leave
		// the process running forever even if cleanup misses it.
		Cmd: exec.Command(termchildPath, "--wait", "--wait-max-duration", "30s"),
	}

	ptp, err := StartProcessWithTerminal(ctx, executor, spec)
	require.NoError(t, err)
	require.NotNil(t, ptp)
	t.Cleanup(func() { _ = ptp.PTY.Close() })

	up, ok := ptp.PTY.(*unixPTY)
	require.True(t, ok, "expected *unixPTY, got %T", ptp.PTY)

	ws, err := pty_get_size(up.master)
	require.NoError(t, err)
	require.Equal(t, defaultInitialCols, ws.cols)
	require.Equal(t, defaultInitialRows, ws.rows)
}

// TestStartProcessWithTerminal_CustomDimensionsApplied verifies that a
// non-zero Cols/Rows spec is propagated to the kernel.
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
		Cmd:  exec.Command(termchildPath, "--wait", "--wait-max-duration", "30s"),
		Cols: wantCols,
		Rows: wantRows,
	}

	ptp, err := StartProcessWithTerminal(ctx, executor, spec)
	require.NoError(t, err)
	require.NotNil(t, ptp)
	t.Cleanup(func() { _ = ptp.PTY.Close() })

	up, ok := ptp.PTY.(*unixPTY)
	require.True(t, ok)

	ws, err := pty_get_size(up.master)
	require.NoError(t, err)
	require.Equal(t, wantCols, ws.cols)
	require.Equal(t, wantRows, ws.rows)
}
