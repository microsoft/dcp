/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	usvc_random "github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

// pickConnMgrSocketPath produces a writable Unix-socket-friendly path that is
// unique to this test invocation. We avoid t.TempDir() because on some
// platforms (notably macOS) the path it yields can exceed the AF_UNIX sun_path
// limit.
func pickConnMgrSocketPath(t *testing.T) string {
	t.Helper()
	suffix, err := usvc_random.MakeRandomString(12)
	require.NoError(t, err)
	root := testutil.TestTempRoot()
	path := filepath.Join(root, fmt.Sprintf("cm-test-%s.sock", suffix))
	t.Cleanup(func() {
		_ = os.Remove(path)
	})
	return path
}

func newConnMgrFixture(t *testing.T) (context.Context, *PseudoTerminalProcess, *internal_testutil.TestPty, string) {
	t.Helper()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	t.Cleanup(cancel)

	pty := internal_testutil.NewTestPty()
	t.Cleanup(func() { _ = pty.Close() })

	ptp := &PseudoTerminalProcess{
		PTY:              pty,
		PID:              process.Pid_t(12345),
		IdentityTime:     time.Now(),
		StartWaitForExit: func() {},
		ExitHandler:      process.NewConcurrentProcessExitHandler(),
	}

	socketPath := pickConnMgrSocketPath(t)
	return ctx, ptp, pty, socketPath
}

// dialConnMgr dials the manager's socket with a deadline derived from ctx and
// fails the test on error.
func dialConnMgr(t *testing.T, ctx context.Context, socketPath string) net.Conn {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTestTimeout)
	}
	d := net.Dialer{Deadline: deadline}
	conn, err := d.DialContext(ctx, "unix", socketPath)
	require.NoErrorf(t, err, "dial unix socket at %s", socketPath)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readHmp1Frame reads one full HMP v1 frame from conn, honoring ctx for the
// read deadline.
func readHmp1Frame(t *testing.T, ctx context.Context, conn net.Conn) (Hmp1FrameType, []byte) {
	t.Helper()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	var header [5]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	length := binary.LittleEndian.Uint32(header[1:5])
	if length == 0 {
		return Hmp1FrameType(header[0]), nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read frame payload (type=0x%02x, len=%d): %v", header[0], length, err)
	}
	return Hmp1FrameType(header[0]), payload
}

func writeHmp1Frame(t *testing.T, conn net.Conn, ft Hmp1FrameType, payload []byte) {
	t.Helper()
	header := [5]byte{byte(ft)}
	binary.LittleEndian.PutUint32(header[1:5], uint32(len(payload)))
	if _, err := conn.Write(header[:]); err != nil {
		t.Fatalf("write frame header: %v", err)
	}
	if len(payload) > 0 {
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write frame payload: %v", err)
		}
	}
}

// drainHelloAndStateSync reads the Hello and StateSync frames that the
// Hmp1Server emits at the start of every session.
func drainHelloAndStateSync(t *testing.T, ctx context.Context, conn net.Conn) Hmp1HelloPayload {
	t.Helper()
	ft, payload := readHmp1Frame(t, ctx, conn)
	if ft != Hmp1FrameHello {
		t.Fatalf("expected Hello, got 0x%02x", ft)
	}
	var hello Hmp1HelloPayload
	require.NoError(t, json.Unmarshal(payload, &hello), "unmarshal Hello payload")

	ft, _ = readHmp1Frame(t, ctx, conn)
	if ft != Hmp1FrameStateSync {
		t.Fatalf("expected StateSync, got 0x%02x", ft)
	}
	return hello
}

// drainUntilExit reads frames from conn until it observes a FrameExit and
// returns its payload. Other frame types are discarded.
func drainUntilExit(t *testing.T, ctx context.Context, conn net.Conn) []byte {
	t.Helper()
	for {
		ft, payload := readHmp1Frame(t, ctx, conn)
		if ft == Hmp1FrameExit {
			return payload
		}
	}
}

// --- Construction errors ---

func TestNewConnManager_RejectsInvalidArgs(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	t.Run("empty socket path in connect mode", func(t *testing.T) {
		// In listen mode an empty path is allowed (DCP generates one); connect mode still requires it.
		_, err := NewConnManager(ctx, ptp, "", SocketModeConnect, 0, 0, log)
		require.Error(t, err)
	})

	t.Run("nil PTY", func(t *testing.T) {
		bad := *ptp
		bad.PTY = nil
		_, err := NewConnManager(ctx, &bad, socketPath, SocketModeListen, 0, 0, log)
		require.Error(t, err)
	})

	t.Run("nil exit handler", func(t *testing.T) {
		bad := *ptp
		bad.ExitHandler = nil
		_, err := NewConnManager(ctx, &bad, socketPath, SocketModeListen, 0, 0, log)
		require.Error(t, err)
	})
}

func TestNewConnManager_FailsWhenSocketPathInUse(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	first, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)
	t.Cleanup(func() { <-first.Done() })

	second, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.Error(t, err, "expected error creating second manager on same socket")
	require.Nil(t, second)
}

// --- Listen-mode socket file ownership ---

// makeStaleSocketFile binds and immediately closes a Unix listener at path
// (opting out of unlink-on-close), leaving a stale socket file behind with no
// live listener, as a crashed process would.
func makeStaleSocketFile(t *testing.T, path string) {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoErrorf(t, err, "create stale socket at %s", path)
	l.SetUnlinkOnClose(false)
	require.NoError(t, l.Close())
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "stale socket file should remain after listener close")
}

// requireFileRemoved asserts that nothing exists at path.
func requireFileRemoved(t *testing.T, path string) {
	t.Helper()
	_, statErr := os.Stat(path)
	require.Truef(t, errors.Is(statErr, os.ErrNotExist), "expected %s to be removed, stat error was %v", path, statErr)
}

// TestConnManager_ListenMode_ReclaimsStaleSocketFile verifies that listen mode
// reclaims a stale leftover socket file before binding and then serves normally.
func TestConnManager_ListenMode_ReclaimsStaleSocketFile(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	makeStaleSocketFile(t, socketPath)

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err, "listen mode should reclaim a stale socket file and bind")
	require.Equal(t, socketPath, cm.SocketPath())

	// The reclaimed socket must serve a fresh client.
	conn := dialConnMgr(t, ctx, socketPath)
	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	writeHmp1Frame(t, conn, Hmp1FrameInput, []byte("ping"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "ping", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for client input to reach PTY")
	}

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

// TestConnManager_ListenMode_DoesNotClobberActiveListener verifies that listen
// mode refuses to bind over an actively-listening socket and leaves it intact.
func TestConnManager_ListenMode_DoesNotClobberActiveListener(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	// A live listener owns the path; it must survive a failed construction.
	startPeerListener(t, socketPath)

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.Error(t, err, "listen mode must not bind over an active listener")
	require.Nil(t, cm)

	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr, "active listener socket file must not be removed")
}

// TestConnManager_ListenMode_RefusesDirectoryPath verifies that listen mode
// never removes a directory that happens to sit at the socket path.
func TestConnManager_ListenMode_RefusesDirectoryPath(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	require.NoError(t, os.Mkdir(socketPath, 0o700))
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.Error(t, err, "listen mode must refuse a directory at the socket path")
	require.Nil(t, cm)

	info, statErr := os.Stat(socketPath)
	require.NoError(t, statErr, "directory must not be removed")
	require.True(t, info.IsDir())
}

// TestConnManager_ListenMode_RemovesSocketFileOnProcessExit verifies that the
// owned socket file is removed when the underlying process exits.
func TestConnManager_ListenMode_RemovesSocketFileOnProcessExit(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)
	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr, "listen mode should create the socket file")

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}

	requireFileRemoved(t, socketPath)
}

// TestConnManager_ListenMode_RemovesSocketFileOnLifetimeCancel verifies that the
// owned socket file is removed when the lifetime context is cancelled.
func TestConnManager_ListenMode_RemovesSocketFileOnLifetimeCancel(t *testing.T) {
	t.Parallel()
	parentCtx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	lifetimeCtx, lifetimeCancel := context.WithCancel(parentCtx)
	defer lifetimeCancel()

	cm, err := NewConnManager(lifetimeCtx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)
	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr)

	lifetimeCancel()
	select {
	case <-cm.Done():
	case <-parentCtx.Done():
		t.Fatal("ConnManager did not become dormant after lifetime cancel")
	}

	requireFileRemoved(t, socketPath)
}

// TestConnManager_ListenMode_ShutdownRemovesSocketFile verifies that the public
// Shutdown() method removes the owned socket file and is idempotent.
func TestConnManager_ListenMode_ShutdownRemovesSocketFile(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)
	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr)

	cm.Shutdown()
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after Shutdown")
	}

	requireFileRemoved(t, socketPath)

	require.NotPanics(t, func() { cm.Shutdown() }, "Shutdown must be idempotent")
}

// TestConnManager_ListenMode_GeneratesSocketPathWhenEmpty verifies that an empty
// socket path in listen mode causes DCP to generate a unique path under the DCP
// temp dir, report it via SocketPath(), serve on it, and remove it on shutdown.
func TestConnManager_ListenMode_GeneratesSocketPathWhenEmpty(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, _ := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, "", SocketModeListen, 0, 0, log)
	require.NoError(t, err, "listen mode must generate a socket path when none is supplied")

	generatedPath := cm.SocketPath()
	require.NotEmpty(t, generatedPath, "generated socket path must be reported via SocketPath()")
	t.Cleanup(func() { _ = os.Remove(generatedPath) })

	require.Truef(t, strings.HasPrefix(generatedPath, usvc_io.DcpTempDir()),
		"generated socket path %q should live under the DCP temp dir %q", generatedPath, usvc_io.DcpTempDir())

	_, statErr := os.Stat(generatedPath)
	require.NoError(t, statErr, "generated socket file should exist while running")

	// It must serve a client like any other listen-mode socket.
	conn := dialConnMgr(t, ctx, generatedPath)
	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}

	requireFileRemoved(t, generatedPath)
}

// --- Normal serve ---

func TestConnManager_AcceptsAndServesClient(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)
	require.Equal(t, socketPath, cm.SocketPath())

	conn := dialConnMgr(t, ctx, socketPath)
	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	// Client sends input; verify it reaches the PTY.
	writeHmp1Frame(t, conn, Hmp1FrameInput, []byte("hello-pty"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "hello-pty", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for client input to reach PTY")
	}

	// PTY produces output; verify it reaches the client as a FrameOutput.
	select {
	case pty.Inbound <- []byte("hello-client"):
	case <-ctx.Done():
		t.Fatal("timed out pushing PTY output")
	}

	ft, payload := readHmp1Frame(t, ctx, conn)
	require.Equal(t, Hmp1FrameOutput, ft)
	require.Equal(t, "hello-client", string(payload))

	// Tear down by simulating process exit; manager should shut down.
	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not transition to dormant state after process exit")
	}
}

func TestConnManager_HelloAdvertisesConfiguredInitialSize(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 132, 50, log)
	require.NoError(t, err)

	conn := dialConnMgr(t, ctx, socketPath)
	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)
	require.Equal(t, uint16(132), hello.Width)
	require.Equal(t, uint16(50), hello.Height)

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not transition to dormant state after process exit")
	}
}

func TestConnManager_ReusableAcrossSequentialConnections(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	runSession := func(name string) {
		conn := dialConnMgr(t, ctx, socketPath)
		drainHelloAndStateSync(t, ctx, conn)

		writeHmp1Frame(t, conn, Hmp1FrameInput, []byte(name))
		select {
		case got := <-pty.Outbound:
			require.Equalf(t, name, string(got), "session %s", name)
		case <-ctx.Done():
			t.Fatalf("session %s: timed out waiting for PTY input", name)
		}

		// Close gracefully on the client side; manager should accept the next.
		require.NoError(t, conn.Close())
	}

	runSession("first")
	runSession("second")
	runSession("third")

	// Shut down by cancelling lifetime via process exit.
	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

func TestConnManager_OnlyOneClientServedAtATime(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)
	_ = cm

	// First client connects and drains the initial frames so we know it is
	// being served.
	first := dialConnMgr(t, ctx, socketPath)
	drainHelloAndStateSync(t, ctx, first)

	// Second client connects. The OS will accept the underlying TCP-like
	// connection but the manager will not call Serve on it until the first
	// session ends. We assert that by trying to read a frame on the second
	// connection with a short deadline; this MUST time out.
	second := dialConnMgr(t, ctx, socketPath)

	require.NoError(t, second.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	var header [5]byte
	_, readErr := io.ReadFull(second, header[:])
	require.Error(t, readErr, "expected second client to not receive any frame yet")
	var netErr net.Error
	require.True(t, errors.As(readErr, &netErr) && netErr.Timeout(),
		"expected timeout error, got %v", readErr)
	require.NoError(t, second.SetReadDeadline(time.Time{}))

	// End first session.
	require.NoError(t, first.Close())

	// Now the second client should get Hello + StateSync.
	drainHelloAndStateSync(t, ctx, second)

	// And it should be able to round-trip input through the PTY.
	writeHmp1Frame(t, second, Hmp1FrameInput, []byte("second"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "second", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for second client's input to reach PTY")
	}
}

// --- Shutdown paths ---

func TestConnManager_ProcessExitShutsDownManager(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	// Process exits BEFORE any client connects.
	ptp.ExitHandler.OnProcessExited(ptp.PID, 7, nil)

	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

func TestConnManager_LifetimeContextCancelShutsDownManager(t *testing.T) {
	t.Parallel()
	parentCtx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	lifetimeCtx, lifetimeCancel := context.WithCancel(parentCtx)
	defer lifetimeCancel()

	cm, err := NewConnManager(lifetimeCtx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	// Connect a client so the manager has a Serve in flight.
	conn := dialConnMgr(t, parentCtx, socketPath)
	drainHelloAndStateSync(t, parentCtx, conn)

	lifetimeCancel()

	// Client should be disconnected (we read frames until Exit / EOF).
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for {
		ft, _ := readHmp1FrameAllowEOF(t, conn)
		if ft == Hmp1FrameExit || ft == 0 {
			break
		}
	}

	select {
	case <-cm.Done():
	case <-parentCtx.Done():
		t.Fatal("ConnManager did not become dormant after lifetime ctx cancel")
	}
}

func TestConnManager_ExitCodePropagatedToClient(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	conn := dialConnMgr(t, ctx, socketPath)
	drainHelloAndStateSync(t, ctx, conn)

	// Process exits with a known exit code. Close the PTY so the server's
	// PTY-read pump unblocks with EOF (mimicking what the OS does when the
	// child closes the slave fd). This triggers the exit-frame send path.
	const wantExit int32 = 99
	ptp.ExitHandler.OnProcessExited(ptp.PID, wantExit, nil)
	_ = pty.Close()

	payload := drainUntilExit(t, ctx, conn)
	require.Len(t, payload, 4, "exit payload must be 4 bytes")
	got := int32(binary.LittleEndian.Uint32(payload))
	require.Equal(t, wantExit, got)

	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

// readHmp1FrameAllowEOF reads a single HMP v1 frame from conn. It returns
// (0, nil) on EOF / closed-connection style errors instead of failing the
// test. Useful for shutdown path tests where the connection may close
// before/after the Exit frame.
func readHmp1FrameAllowEOF(t *testing.T, conn net.Conn) (Hmp1FrameType, []byte) {
	t.Helper()
	var header [5]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
			errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			return 0, nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return 0, nil
		}
		t.Fatalf("read frame header: %v", err)
	}
	length := binary.LittleEndian.Uint32(header[1:5])
	if length == 0 {
		return Hmp1FrameType(header[0]), nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	return Hmp1FrameType(header[0]), payload
}

// --- Connect mode ---

// startPeerListener binds a Unix listener at socketPath that a connect-mode
// ConnManager will dial into. In connect mode the peer owns the socket file, so
// the listener (and the file) are created and cleaned up by the test, not by
// ConnManager.
func startPeerListener(t *testing.T, socketPath string) *net.UnixListener {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	require.NoErrorf(t, err, "listen unix socket at %s", socketPath)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// acceptPeer accepts a single connection on the peer listener, honoring ctx for
// the accept deadline, and fails the test on error.
func acceptPeer(t *testing.T, ctx context.Context, l *net.UnixListener) net.Conn {
	t.Helper()
	if deadline, ok := ctx.Deadline(); ok {
		_ = l.SetDeadline(deadline)
	}
	conn, err := l.AcceptUnix()
	require.NoError(t, err, "accept peer connection")
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestConnManager_ConnectModeDialsPeerAndServes(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	peer := startPeerListener(t, socketPath)

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, 0, 0, log)
	require.NoError(t, err)
	require.Equal(t, socketPath, cm.SocketPath())

	conn := acceptPeer(t, ctx, peer)
	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	// Client sends input; verify it reaches the PTY.
	writeHmp1Frame(t, conn, Hmp1FrameInput, []byte("hello-pty"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "hello-pty", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for client input to reach PTY")
	}

	// PTY produces output; verify it reaches the client as a FrameOutput.
	select {
	case pty.Inbound <- []byte("hello-client"):
	case <-ctx.Done():
		t.Fatal("timed out pushing PTY output")
	}

	ft, payload := readHmp1Frame(t, ctx, conn)
	require.Equal(t, Hmp1FrameOutput, ft)
	require.Equal(t, "hello-client", string(payload))

	// Tear down by simulating process exit; manager should shut down.
	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not transition to dormant state after process exit")
	}
}

func TestConnManager_ConnectModeStartsWithoutPeerThenDials(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	// No peer is listening yet: construction must still succeed, and the
	// manager must keep retrying the dial until a peer appears.
	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, 0, 0, log)
	require.NoError(t, err)

	// Connect mode must not create the socket file; the peer owns it.
	_, statErr := os.Stat(socketPath)
	require.True(t, errors.Is(statErr, os.ErrNotExist), "connect mode must not create the socket file")

	// Now bring up the peer; the manager should dial in and complete the handshake.
	peer := startPeerListener(t, socketPath)
	conn := acceptPeer(t, ctx, peer)
	drainHelloAndStateSync(t, ctx, conn)

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

func TestConnManager_ConnectModeReconnectsAfterSessionEnds(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	peer := startPeerListener(t, socketPath)

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, 0, 0, log)
	require.NoError(t, err)

	runSession := func(name string) {
		conn := acceptPeer(t, ctx, peer)
		drainHelloAndStateSync(t, ctx, conn)

		writeHmp1Frame(t, conn, Hmp1FrameInput, []byte(name))
		select {
		case got := <-pty.Outbound:
			require.Equalf(t, name, string(got), "session %s", name)
		case <-ctx.Done():
			t.Fatalf("session %s: timed out waiting for PTY input", name)
		}

		// Close from the peer side; the manager should re-dial for the next session.
		require.NoError(t, conn.Close())
	}

	runSession("first")
	runSession("second")
	runSession("third")

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

func TestConnManager_ConnectModeLifetimeCancelWhileDialingShutsDown(t *testing.T) {
	t.Parallel()
	parentCtx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	lifetimeCtx, lifetimeCancel := context.WithCancel(parentCtx)
	defer lifetimeCancel()

	// No peer ever appears: the manager is stuck retrying the dial.
	cm, err := NewConnManager(lifetimeCtx, ptp, socketPath, SocketModeConnect, 0, 0, log)
	require.NoError(t, err)

	lifetimeCancel()

	select {
	case <-cm.Done():
	case <-parentCtx.Done():
		t.Fatal("ConnManager did not become dormant after lifetime ctx cancel while dialing")
	}
}

func TestConnManager_ConnectModeProcessExitWhileDialingShutsDown(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	// No peer is listening; the manager is retrying the dial.
	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, 0, 0, log)
	require.NoError(t, err)

	ptp.ExitHandler.OnProcessExited(ptp.PID, 7, nil)

	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit while dialing")
	}
}

func TestConnManager_ConnectModeAcceptThenCloseDoesNotHotLoop(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	peer := startPeerListener(t, socketPath)

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, 0, 0, log)
	require.NoError(t, err)

	// Peer accepts each dial and immediately closes it. This drives the
	// manager's tightest reconnect path; the per-redial minimum interval must
	// keep the accept rate bounded.
	var accepts atomic.Int64
	stop := make(chan struct{})
	var peerWg sync.WaitGroup
	peerWg.Add(1)
	go func() {
		defer peerWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = peer.SetDeadline(time.Now().Add(200 * time.Millisecond))
			conn, acceptErr := peer.AcceptUnix()
			if acceptErr != nil {
				select {
				case <-stop:
					return
				default:
					continue
				}
			}
			accepts.Add(1)
			_ = conn.Close()
		}
	}()

	// Observe for a window spanning several redial intervals.
	const window = 4 * reconnectMinInterval
	select {
	case <-time.After(window):
	case <-ctx.Done():
		t.Fatal("test context expired before observation window elapsed")
	}

	close(stop)
	peerWg.Wait()

	// With a minimum interval between redials, the number of accepts over the
	// window must be bounded well below a hot-loop. Allow generous headroom for
	// scheduling jitter while still proving redials are throttled.
	got := accepts.Load()
	maxExpected := int64(window/reconnectMinInterval) + 3
	require.LessOrEqualf(t, got, maxExpected,
		"connect mode hot-looped: %d accepts in %s (max expected %d)", got, window, maxExpected)

	// Shut down cleanly.
	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

// --- Attach-process (pre-connect) mode ---

// requireNoFrameYet asserts that no HMP v1 frame is currently readable from conn
// within a short window. Used to prove that a connection warmed before the
// process is attached has not yet started the handshake (no Hmp1Server exists).
func requireNoFrameYet(t *testing.T, conn net.Conn) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(500*time.Millisecond)))
	var header [5]byte
	_, readErr := io.ReadFull(conn, header[:])
	require.Error(t, readErr, "expected no frame before the process is attached")
	var netErr net.Error
	require.Truef(t, errors.As(readErr, &netErr) && netErr.Timeout(),
		"expected a timeout error, got %v", readErr)
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
}

func TestConnManager_AttachProcessListenMode_WarmsThenServesAfterAttach(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	// Attach-process mode: nil ptp. The manager binds the listener and warms a
	// connection in the background before any process exists.
	cm, err := NewConnManager(ctx, nil, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)
	require.Equal(t, socketPath, cm.SocketPath())

	// The socket file must exist immediately so a client can connect before the
	// process is attached.
	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr, "listen-mode socket file should exist before attach")

	// Client connects (warms the connection). The manager must NOT send Hello yet
	// because there is no process/server.
	conn := dialConnMgr(t, ctx, socketPath)
	requireNoFrameYet(t, conn)

	// Supply the process; the manager now serves the warmed connection.
	require.NoError(t, cm.AttachProcess(ptp))

	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	// Client input reaches the PTY.
	writeHmp1Frame(t, conn, Hmp1FrameInput, []byte("hello-pty"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "hello-pty", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for client input to reach PTY")
	}

	// PTY output reaches the client.
	select {
	case pty.Inbound <- []byte("hello-client"):
	case <-ctx.Done():
		t.Fatal("timed out pushing PTY output")
	}
	ft, payload := readHmp1Frame(t, ctx, conn)
	require.Equal(t, Hmp1FrameOutput, ft)
	require.Equal(t, "hello-client", string(payload))

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not transition to dormant state after process exit")
	}
}

func TestConnManager_AttachProcessConnectMode_WarmsThenServesAfterAttach(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	peer := startPeerListener(t, socketPath)

	cm, err := NewConnManager(ctx, nil, socketPath, SocketModeConnect, 0, 0, log)
	require.NoError(t, err)

	// The manager pre-dials the peer; accepting proves the warmed connection.
	conn := acceptPeer(t, ctx, peer)
	requireNoFrameYet(t, conn)

	require.NoError(t, cm.AttachProcess(ptp))

	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	writeHmp1Frame(t, conn, Hmp1FrameInput, []byte("hello-pty"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "hello-pty", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for client input to reach PTY")
	}

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not transition to dormant state after process exit")
	}
}

func TestConnManager_AttachProcessMode_AttachBeforeClientConnects(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, nil, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	// Attach the process before any client has connected: it parks in the
	// hand-off channel and is served as soon as a client appears.
	require.NoError(t, cm.AttachProcess(ptp))

	conn := dialConnMgr(t, ctx, socketPath)
	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	writeHmp1Frame(t, conn, Hmp1FrameInput, []byte("after-attach"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "after-attach", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for client input to reach PTY")
	}

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

func TestConnManager_AttachProcessMode_ReconnectsAfterFirstSession(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, nil, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	// Warm the first connection, then attach the process.
	first := dialConnMgr(t, ctx, socketPath)
	require.NoError(t, cm.AttachProcess(ptp))
	drainHelloAndStateSync(t, ctx, first)

	writeHmp1Frame(t, first, Hmp1FrameInput, []byte("first"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "first", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for first session input")
	}
	require.NoError(t, first.Close())

	// After the warmed session ends, the manager must serve a fresh client.
	second := dialConnMgr(t, ctx, socketPath)
	drainHelloAndStateSync(t, ctx, second)
	writeHmp1Frame(t, second, Hmp1FrameInput, []byte("second"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "second", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for second session input")
	}

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

func TestConnManager_AttachProcessMode_HelloAdvertisesConfiguredSize(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, nil, socketPath, SocketModeListen, 132, 50, log)
	require.NoError(t, err)

	conn := dialConnMgr(t, ctx, socketPath)
	require.NoError(t, cm.AttachProcess(ptp))

	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, uint16(132), hello.Width)
	require.Equal(t, uint16(50), hello.Height)

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

func TestConnManager_AttachProcessMode_LifetimeCancelBeforeConnect(t *testing.T) {
	t.Parallel()
	parentCtx, _, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	lifetimeCtx, lifetimeCancel := context.WithCancel(parentCtx)
	defer lifetimeCancel()

	cm, err := NewConnManager(lifetimeCtx, nil, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	// No client ever connects; cancelling the lifetime must shut the manager down.
	lifetimeCancel()

	select {
	case <-cm.Done():
	case <-parentCtx.Done():
		t.Fatal("ConnManager did not become dormant after lifetime cancel before connect")
	}
}

func TestConnManager_AttachProcessMode_LifetimeCancelAfterConnectBeforeAttach(t *testing.T) {
	t.Parallel()
	parentCtx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	lifetimeCtx, lifetimeCancel := context.WithCancel(parentCtx)
	defer lifetimeCancel()

	cm, err := NewConnManager(lifetimeCtx, nil, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	// Warm a connection but do not attach a process.
	conn := dialConnMgr(t, parentCtx, socketPath)
	requireNoFrameYet(t, conn)

	lifetimeCancel()

	select {
	case <-cm.Done():
	case <-parentCtx.Done():
		t.Fatal("ConnManager did not become dormant after lifetime cancel before attach")
	}

	// Attaching after shutdown must fail.
	require.Error(t, cm.AttachProcess(ptp))
}

func TestConnManager_AttachProcessMode_ClientDisconnectsBeforeAttachThenRecovers(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, nil, socketPath, SocketModeListen, 0, 0, log)
	require.NoError(t, err)

	// Warm a connection, then drop it before attaching the process.
	warm := dialConnMgr(t, ctx, socketPath)
	requireNoFrameYet(t, warm)
	require.NoError(t, warm.Close())

	// Attach the process. The warmed connection is dead, so the manager fails
	// fast serving it and falls back to accepting a fresh client.
	require.NoError(t, cm.AttachProcess(ptp))

	fresh := dialConnMgr(t, ctx, socketPath)
	drainHelloAndStateSync(t, ctx, fresh)
	writeHmp1Frame(t, fresh, Hmp1FrameInput, []byte("recovered"))
	select {
	case got := <-pty.Outbound:
		require.Equal(t, "recovered", string(got))
	case <-ctx.Done():
		t.Fatal("timed out waiting for fresh client input after recovery")
	}

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}

func TestConnManager_AttachProcessValidations(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, _ := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	t.Run("eager manager rejects attach", func(t *testing.T) {
		cm, err := NewConnManager(ctx, ptp, pickConnMgrSocketPath(t), SocketModeListen, 0, 0, log)
		require.NoError(t, err)
		require.Error(t, cm.AttachProcess(ptp))
	})

	t.Run("nil ptp", func(t *testing.T) {
		cm, err := NewConnManager(ctx, nil, pickConnMgrSocketPath(t), SocketModeListen, 0, 0, log)
		require.NoError(t, err)
		require.Error(t, cm.AttachProcess(nil))
	})

	t.Run("nil PTY", func(t *testing.T) {
		cm, err := NewConnManager(ctx, nil, pickConnMgrSocketPath(t), SocketModeListen, 0, 0, log)
		require.NoError(t, err)
		bad := *ptp
		bad.PTY = nil
		require.Error(t, cm.AttachProcess(&bad))
	})

	t.Run("nil exit handler", func(t *testing.T) {
		cm, err := NewConnManager(ctx, nil, pickConnMgrSocketPath(t), SocketModeListen, 0, 0, log)
		require.NoError(t, err)
		bad := *ptp
		bad.ExitHandler = nil
		require.Error(t, cm.AttachProcess(&bad))
	})

	t.Run("double attach", func(t *testing.T) {
		cm, err := NewConnManager(ctx, nil, pickConnMgrSocketPath(t), SocketModeListen, 0, 0, log)
		require.NoError(t, err)
		require.NoError(t, cm.AttachProcess(ptp))
		require.Error(t, cm.AttachProcess(ptp))
	})
}

func TestConnManager_AttachProcessConnectMode_RedialThrottledAfterImmediateClose(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	peer := startPeerListener(t, socketPath)

	cm, err := NewConnManager(ctx, nil, socketPath, SocketModeConnect, 0, 0, log)
	require.NoError(t, err)

	var accepts atomic.Int64

	// Accept the warmed dial, attach the process, then close it. Every
	// subsequent dial is likewise accepted and closed immediately. The first
	// reconnect follows the warmed connection, so this exercises that the
	// per-redial minimum interval is carried over into the normal loop.
	warm := acceptPeer(t, ctx, peer)
	accepts.Add(1)
	require.NoError(t, cm.AttachProcess(ptp))
	require.NoError(t, warm.Close())

	stop := make(chan struct{})
	var peerWg sync.WaitGroup
	peerWg.Add(1)
	go func() {
		defer peerWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = peer.SetDeadline(time.Now().Add(200 * time.Millisecond))
			conn, acceptErr := peer.AcceptUnix()
			if acceptErr != nil {
				select {
				case <-stop:
					return
				default:
					continue
				}
			}
			accepts.Add(1)
			_ = conn.Close()
		}
	}()

	const window = 4 * reconnectMinInterval
	select {
	case <-time.After(window):
	case <-ctx.Done():
		t.Fatal("test context expired before observation window elapsed")
	}

	close(stop)
	peerWg.Wait()

	got := accepts.Load()
	maxExpected := int64(window/reconnectMinInterval) + 4
	require.LessOrEqualf(t, got, maxExpected,
		"attach-process connect mode hot-looped: %d accepts in %s (max expected %d)", got, window, maxExpected)

	ptp.ExitHandler.OnProcessExited(ptp.PID, 0, nil)
	select {
	case <-cm.Done():
	case <-ctx.Done():
		t.Fatal("ConnManager did not become dormant after process exit")
	}
}
