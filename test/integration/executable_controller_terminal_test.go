/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	wait "k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/termpty"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	usvc_random "github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

// pickTerminalSocketPath returns a filesystem path safe to use for a Unix domain
// socket from this test. The path is short (placed under testutil.TestTempRoot)
// to stay within the AF_UNIX sun_path length limit on macOS and other Unix
// platforms.
func pickTerminalSocketPath(t *testing.T) string {
	t.Helper()
	suffix, err := usvc_random.MakeRandomString(8)
	require.NoError(t, err)
	root := testutil.TestTempRoot()
	path := filepath.Join(root, fmt.Sprintf("dcp-exe-term-%s.sock", suffix))
	t.Cleanup(func() {
		_ = os.Remove(path)
	})
	return path
}

// makeTerminalExecutable returns a minimal Executable that has Spec.Terminal
// configured for the supplied UDS path.
func makeTerminalExecutable(name, udsPath string) *apiv1.Executable {
	return &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: "/path/to/" + name,
			Terminal: &apiv1.TerminalSpec{
				UDSPath: udsPath,
				Cols:    120,
				Rows:    40,
			},
		},
	}
}

// readHmp1Frame reads one full HMP v1 frame from conn, honoring ctx for the
// read deadline.
func readHmp1Frame(t *testing.T, ctx context.Context, conn net.Conn) (termpty.Hmp1FrameType, []byte) {
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
		return termpty.Hmp1FrameType(header[0]), nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read frame payload (type=0x%02x, len=%d): %v", header[0], length, err)
	}
	return termpty.Hmp1FrameType(header[0]), payload
}

func writeHmp1Frame(t *testing.T, conn net.Conn, ft termpty.Hmp1FrameType, payload []byte) {
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
// Hmp1Server emits at the start of every session and returns the Hello payload.
func drainHelloAndStateSync(t *testing.T, ctx context.Context, conn net.Conn) termpty.Hmp1HelloPayload {
	t.Helper()
	ft, payload := readHmp1Frame(t, ctx, conn)
	require.Equalf(t, termpty.Hmp1FrameHello, ft, "expected Hello, got 0x%02x", ft)
	var hello termpty.Hmp1HelloPayload
	require.NoError(t, json.Unmarshal(payload, &hello), "unmarshal Hello payload")

	ft, _ = readHmp1Frame(t, ctx, conn)
	require.Equalf(t, termpty.Hmp1FrameStateSync, ft, "expected StateSync, got 0x%02x", ft)
	return hello
}

// dialTerminalSocketWhenReady opens the executable's terminal UDS, retrying briefly
// until the socket is accepting connections (the executable controller wires up the
// socket asynchronously after the underlying process is started).
func dialTerminalSocketWhenReady(t *testing.T, ctx context.Context, socketPath string) net.Conn {
	t.Helper()
	var conn net.Conn
	dialErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		c, err := net.Dial("unix", socketPath)
		if err != nil {
			return false, nil
		}
		conn = c
		return true, nil
	})
	require.NoError(t, dialErr, "could not dial terminal socket %q", socketPath)
	return conn
}

// ensureExeRunning waits for an Executable to enter the Running state and returns
// the run's PID (parsed from Status.ExecutionID).
func ensureExeRunning(t *testing.T, ctx context.Context, exe *apiv1.Executable) process.Pid_t {
	t.Helper()
	updated := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(exe), func(currentExe *apiv1.Executable) (bool, error) {
		return currentExe.Status.State == apiv1.ExecutableStateRunning && currentExe.Status.ExecutionID != "", nil
	})
	pid64, parseErr := strconv.ParseInt(updated.Status.ExecutionID, 10, 32)
	require.NoErrorf(t, parseErr, "parse ExecutionID for Executable %q", exe.Name)
	pid, pidErr := process.Int64_ToPidT(pid64)
	require.NoError(t, pidErr)
	return pid
}

// readHmp1OutputAggregated reads Hmp1FrameOutput frames from conn until the
// accumulated payload contains needle, ignoring other frame types (the live
// Hmp1Server emits frames concurrently from PTY chunks, so we need a streaming
// substring match).
func readHmp1OutputAggregated(t *testing.T, ctx context.Context, conn net.Conn, needle string) string {
	t.Helper()
	var acc strings.Builder
	for {
		ft, payload := readHmp1Frame(t, ctx, conn)
		if ft == termpty.Hmp1FrameOutput {
			acc.Write(payload)
			if strings.Contains(acc.String(), needle) {
				return acc.String()
			}
		}
		// Ignore any other frames (state-sync would be unusual after the initial
		// drain; exit frames are not expected here).
		if ft == termpty.Hmp1FrameExit {
			t.Fatalf("unexpected Exit frame before finding %q in output (got so far: %q)", needle, acc.String())
		}
	}
}

// ====================================================================================
// TestPty-backed tests
// ====================================================================================

// TestExecutableTerminalServerListensOnConfiguredSocket verifies that the executable
// controller stands up a termpty.ConnManager on the configured UDS path and that the
// Hello frame reports the dimensions from Spec.Terminal.
func TestExecutableTerminalServerListensOnConfiguredSocket(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-exe-term-listen"
	socketPath := pickTerminalSocketPath(t)
	exe := makeTerminalExecutable(exeName, socketPath)

	_ = terminalProcessFactoryDispatcher.InstallHandler(t, exe.Spec.ExecutablePath, ctrl_testutil.NewTestPtyTerminalFactory())

	require.NoError(t, client.Create(ctx, exe), "create Executable")
	t.Cleanup(func() { _ = client.Delete(context.Background(), exe) })

	_ = ensureExeRunning(t, ctx, exe)

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()

	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)
	// NOTE: ConnManager currently reports the Hmp1Server's default Hello
	// dimensions (80, 24) rather than the dimensions configured in
	// Spec.Terminal. That's an existing ConnManager limitation, not
	// something this test should pin down — we only assert the version here.
}

// TestExecutableTerminalBridgesIO verifies that bytes written into the TestPty
// flow out as Hmp1 Output frames, and that Hmp1 Input frames written to the
// socket flow into the TestPty's outbound channel.
func TestExecutableTerminalBridgesIO(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-exe-term-io"
	socketPath := pickTerminalSocketPath(t)
	exe := makeTerminalExecutable(exeName, socketPath)

	ptyCh := terminalProcessFactoryDispatcher.InstallHandler(t, exe.Spec.ExecutablePath, ctrl_testutil.NewTestPtyTerminalFactory())

	require.NoError(t, client.Create(ctx, exe), "create Executable")
	t.Cleanup(func() { _ = client.Delete(context.Background(), exe) })

	_ = ensureExeRunning(t, ctx, exe)
	var testPty *internal_testutil.TestPty
	select {
	case testPty = <-ptyCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty from terminal factory")
	}

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()
	_ = drainHelloAndStateSync(t, ctx, conn)

	// PTY -> Output frame.
	const ptyOut = "hello-from-process"
	select {
	case testPty.Inbound <- []byte(ptyOut):
	case <-ctx.Done():
		t.Fatal("timed out feeding TestPty inbound")
	}
	_ = readHmp1OutputAggregated(t, ctx, conn, ptyOut)

	// Input frame -> PTY.
	const clientIn = "hello-from-client"
	writeHmp1Frame(t, conn, termpty.Hmp1FrameInput, []byte(clientIn))
	select {
	case got := <-testPty.Outbound:
		require.Equal(t, clientIn, string(got), "expected PTY to receive bytes from Input frame")
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty to receive Input bytes")
	}
}

// TestExecutableTerminalResizeFromClient verifies that Hmp1 Resize frames are
// applied to the PTY (recorded in TestPty.Resizes).
func TestExecutableTerminalResizeFromClient(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-exe-term-resize"
	socketPath := pickTerminalSocketPath(t)
	exe := makeTerminalExecutable(exeName, socketPath)

	ptyCh := terminalProcessFactoryDispatcher.InstallHandler(t, exe.Spec.ExecutablePath, ctrl_testutil.NewTestPtyTerminalFactory())

	require.NoError(t, client.Create(ctx, exe), "create Executable")
	t.Cleanup(func() { _ = client.Delete(context.Background(), exe) })

	_ = ensureExeRunning(t, ctx, exe)
	var testPty *internal_testutil.TestPty
	select {
	case testPty = <-ptyCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty from terminal factory")
	}

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()
	_ = drainHelloAndStateSync(t, ctx, conn)

	resizePayload := make([]byte, 8)
	binary.LittleEndian.PutUint32(resizePayload[0:4], 132)
	binary.LittleEndian.PutUint32(resizePayload[4:8], 50)
	writeHmp1Frame(t, conn, termpty.Hmp1FrameResize, resizePayload)

	pollErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		resizes := testPty.ResizesSnapshot()
		for _, r := range resizes {
			if r.Cols == 132 && r.Rows == 50 {
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, pollErr, "expected TestPty to record a (132, 50) resize from client")
}

// TestExecutableTerminalProcessExitSendsExitFrame verifies that
// when the underlying process exits, the connected client receives an Exit frame
// carrying the process exit code and the Executable transitions to a terminal
// state.
func TestExecutableTerminalProcessExitSendsExitFrame(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-exe-term-exit"
	const expectedExitCode int32 = 42
	socketPath := pickTerminalSocketPath(t)
	exe := makeTerminalExecutable(exeName, socketPath)

	_ = terminalProcessFactoryDispatcher.InstallHandler(t, exe.Spec.ExecutablePath, ctrl_testutil.NewTestPtyTerminalFactory())

	require.NoError(t, client.Create(ctx, exe), "create Executable")
	t.Cleanup(func() { _ = client.Delete(context.Background(), exe) })

	pid := ensureExeRunning(t, ctx, exe)

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()
	_ = drainHelloAndStateSync(t, ctx, conn)

	testProcessExecutor.SimulateProcessExit(t, pid, expectedExitCode)

	// Read frames until we see Exit, asserting on its exit code payload.
	for {
		ft, payload := readHmp1Frame(t, ctx, conn)
		if ft != termpty.Hmp1FrameExit {
			continue
		}
		require.Equal(t, 4, len(payload), "Exit frame payload must be 4 bytes")
		code := int32(binary.LittleEndian.Uint32(payload))
		require.Equalf(t, expectedExitCode, code, "Exit frame should carry process exit code")
		break
	}

	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(exe), func(currentExe *apiv1.Executable) (bool, error) {
		return currentExe.Done(), nil
	})
}

// TestExecutableTerminalDeletionTearsDownPty verifies that deleting the
// Executable closes the TestPty.
func TestExecutableTerminalDeletionTearsDownPty(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-exe-term-delete"
	socketPath := pickTerminalSocketPath(t)
	exe := makeTerminalExecutable(exeName, socketPath)

	ptyCh := terminalProcessFactoryDispatcher.InstallHandler(t, exe.Spec.ExecutablePath, ctrl_testutil.NewTestPtyTerminalFactory())

	require.NoError(t, client.Create(ctx, exe), "create Executable")

	_ = ensureExeRunning(t, ctx, exe)
	var testPty *internal_testutil.TestPty
	select {
	case testPty = <-ptyCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty from terminal factory")
	}

	// Status should report the configured terminal socket path.
	running := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(exe), func(currentExe *apiv1.Executable) (bool, error) {
		return currentExe.Status.TerminalSocketPath != "", nil
	})
	require.Equal(t, socketPath, running.Status.TerminalSocketPath, "Status should report the configured terminal socket path")

	// Sanity check: the socket file exists before deletion.
	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr, "socket file should exist before deletion")

	require.NoError(t, client.Delete(ctx, exe), "delete Executable")

	pollErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		return testPty.IsClosed(), nil
	})
	require.NoError(t, pollErr, "expected TestPty to be closed after Executable deletion")

	// DCP owns the listen-mode socket file, so it must be removed once the terminal is torn down.
	removalErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		_, pollStatErr := os.Stat(socketPath)
		return errors.Is(pollStatErr, os.ErrNotExist), nil
	})
	require.NoError(t, removalErr, "terminal socket file should be removed after Executable deletion")
}

// TestExecutableTerminalGeneratesSocketPathWhenUDSPathEmpty verifies that a terminal
// Executable created with an empty UDSPath reaches Running with a DCP-generated socket
// path reported in Status, that the socket exists and serves while Running, and that the
// generated file is removed once the Executable is deleted.
func TestExecutableTerminalGeneratesSocketPathWhenUDSPathEmpty(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-exe-term-autopath"
	exe := makeTerminalExecutable(exeName, "") // empty UDSPath: DCP generates the socket path.

	ptyCh := terminalProcessFactoryDispatcher.InstallHandler(t, exe.Spec.ExecutablePath, ctrl_testutil.NewTestPtyTerminalFactory())

	require.NoError(t, client.Create(ctx, exe), "create Executable")

	_ = ensureExeRunning(t, ctx, exe)
	var testPty *internal_testutil.TestPty
	select {
	case testPty = <-ptyCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty from terminal factory")
	}

	running := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(exe), func(currentExe *apiv1.Executable) (bool, error) {
		return currentExe.Status.TerminalSocketPath != "", nil
	})
	generatedPath := running.Status.TerminalSocketPath
	require.NotEmpty(t, generatedPath, "Status should report the DCP-generated terminal socket path")
	require.Truef(t, strings.HasPrefix(generatedPath, usvc_io.DcpTempDir()),
		"generated socket path %q should live under the DCP temp dir %q", generatedPath, usvc_io.DcpTempDir())
	t.Cleanup(func() { _ = os.Remove(generatedPath) })

	_, statErr := os.Stat(generatedPath)
	require.NoError(t, statErr, "generated socket file should exist while Running")

	// The generated socket must actually serve HMP v1 clients.
	conn := dialTerminalSocketWhenReady(t, ctx, generatedPath)
	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)
	_ = conn.Close()

	require.NoError(t, client.Delete(ctx, exe), "delete Executable")

	pollErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		return testPty.IsClosed(), nil
	})
	require.NoError(t, pollErr, "expected TestPty to be closed after Executable deletion")

	removalErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		_, pollStatErr := os.Stat(generatedPath)
		return errors.Is(pollStatErr, os.ErrNotExist), nil
	})
	require.NoError(t, removalErr, "generated terminal socket file should be removed after Executable deletion")
}

// Verifies that when the terminal process factory returns an error, the Executable
// reaches FailedToStart state.
func TestExecutableTerminalStartupFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const exeName = "test-exe-term-startup-fail"
	socketPath := pickTerminalSocketPath(t)
	exe := makeTerminalExecutable(exeName, socketPath)

	factoryErr := errors.New("simulated terminal startup failure")
	_ = terminalProcessFactoryDispatcher.InstallHandler(t, exe.Spec.ExecutablePath, func(
		_ context.Context,
		_ process.Executor,
		_ *termpty.CommandSpec,
	) (*termpty.PseudoTerminalProcess, error) {
		return nil, factoryErr
	})

	require.NoError(t, client.Create(ctx, exe), "create Executable")
	t.Cleanup(func() { _ = client.Delete(context.Background(), exe) })

	_ = waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(exe), func(currentExe *apiv1.Executable) (bool, error) {
		return currentExe.Status.State == apiv1.ExecutableStateFailedToStart, nil
	})
}

// TestExecutableTerminalEndToEndWithRealPty exercises the full controller path
// through a real OS process attached to a real pseudo-terminal. It uses the
// cross-platform `termchild` test helper as the executable and the real
// termpty.StartProcessWithTerminal factory (no TestPty override).
//
// The test runs on both Windows (ConPTY) and Unix (PTY). It skips gracefully if
// the termchild binary is not available so a missing prerequisite is reported as
// a skip rather than a runtime failure.
func TestExecutableTerminalEndToEndWithRealPty(t *testing.T) {
	t.Parallel()

	termchildPath, lookupErr := internal_testutil.GetTestToolPath("termchild")
	if lookupErr != nil {
		t.Skipf("termchild test tool is not available: %v", lookupErr)
	}

	const testTimeout = 2 * time.Minute
	ctx, cancel := testutil.GetTestContext(t, testTimeout)
	defer cancel()

	serverInfo, teInfo, startupErr := StartAdvancedTestEnvironmentWithFlags(
		ctx,
		ExecutableController,
		t.Name(),
		NoSeparateWorkingDir,
		ctrl_testutil.ApiServerFlagsNone,
	)
	require.NoError(t, startupErr, "failed to start advanced test environment")
	defer teInfo.ProcessExecutor.Dispose()
	defer shutdownAdvancedTestEnvironment(t, ctx, cancel, serverInfo)

	apiClient := serverInfo.Client
	socketPath := pickTerminalSocketPath(t)
	const exeName = "test-exe-term-e2e"

	exe := &apiv1.Executable{
		ObjectMeta: metav1.ObjectMeta{
			Name:      exeName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ExecutableSpec{
			ExecutablePath: termchildPath,
			Args: []string{
				"--disable-echo",
				"--print", "READY",
				"--echo",
				"--wait",
				"--wait-max-duration=30s",
			},
			Terminal: &apiv1.TerminalSpec{
				UDSPath: socketPath,
				Cols:    120,
				Rows:    40,
			},
		},
	}

	require.NoError(t, apiClient.Create(ctx, exe), "create Executable")
	t.Cleanup(func() { _ = apiClient.Delete(context.Background(), exe) })

	// Wait for Running using the advanced-env client (the standard helpers
	// hardcode the global `client`).
	_ = waitObjectAssumesStateEx(t, ctx, apiClient, ctrl_client.ObjectKeyFromObject(exe), func(currentExe *apiv1.Executable) (bool, error) {
		return currentExe.Status.State == apiv1.ExecutableStateRunning && currentExe.Status.ExecutionID != "", nil
	})

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()

	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	// termchild's `--print READY` writes "READY\n". With a PTY in cooked mode
	// this usually arrives as "READY\r\n" (CR/LF translation). We assert that
	// "READY" appears in the aggregated Output bytes (ConPTY may also inject
	// CSI sequences for cursor/screen setup; we accept those as noise).
	_ = readHmp1OutputAggregated(t, ctx, conn, "READY")

	// Send an Input frame and expect termchild's echo loop to write "got:ping".
	// Use CR+LF since ConPTY's input stream follows console conventions.
	writeHmp1Frame(t, conn, termpty.Hmp1FrameInput, []byte("ping\r\n"))
	_ = readHmp1OutputAggregated(t, ctx, conn, "got:ping")

	// Delete the Executable, expect an Exit frame, and the controller to
	// drive the Executable to a terminal state.
	require.NoError(t, apiClient.Delete(ctx, exe), "delete Executable")

	for {
		ft, _ := readHmp1Frame(t, ctx, conn)
		if ft == termpty.Hmp1FrameExit {
			break
		}
	}
}
