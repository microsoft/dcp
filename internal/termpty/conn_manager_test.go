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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	internal_testutil "github.com/microsoft/dcp/internal/testutil"
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

	t.Run("nil ptp", func(t *testing.T) {
		_, err := NewConnManager(ctx, nil, socketPath, SocketModeListen, log)
		require.Error(t, err)
	})

	t.Run("empty socket path", func(t *testing.T) {
		_, err := NewConnManager(ctx, ptp, "", SocketModeListen, log)
		require.Error(t, err)
	})

	t.Run("nil PTY", func(t *testing.T) {
		bad := *ptp
		bad.PTY = nil
		_, err := NewConnManager(ctx, &bad, socketPath, SocketModeListen, log)
		require.Error(t, err)
	})

	t.Run("nil exit handler", func(t *testing.T) {
		bad := *ptp
		bad.ExitHandler = nil
		_, err := NewConnManager(ctx, &bad, socketPath, SocketModeListen, log)
		require.Error(t, err)
	})
}

func TestNewConnManager_FailsWhenSocketPathInUse(t *testing.T) {
	t.Parallel()
	ctx, ptp, _, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	first, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, log)
	require.NoError(t, err)
	t.Cleanup(func() { <-first.Done() })

	second, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, log)
	require.Error(t, err, "expected error creating second manager on same socket")
	require.Nil(t, second)
}

// --- Normal serve ---

func TestConnManager_AcceptsAndServesClient(t *testing.T) {
	t.Parallel()
	ctx, ptp, pty, socketPath := newConnMgrFixture(t)
	log := testutil.NewLogForTesting(t.Name())

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, log)
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

	ptp.InitialCols = 132
	ptp.InitialRows = 50

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, log)
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

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, log)
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

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, log)
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

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, log)
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

	cm, err := NewConnManager(lifetimeCtx, ptp, socketPath, SocketModeListen, log)
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

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeListen, log)
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

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, log)
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
	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, log)
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

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, log)
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
	cm, err := NewConnManager(lifetimeCtx, ptp, socketPath, SocketModeConnect, log)
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
	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, log)
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

	cm, err := NewConnManager(ctx, ptp, socketPath, SocketModeConnect, log)
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
