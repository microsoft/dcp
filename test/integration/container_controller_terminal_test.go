/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package integration_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	wait "k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/termpty"
	ctrl_testutil "github.com/microsoft/dcp/internal/testutil/ctrlutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	usvc_random "github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

// pickContainerTerminalSocketPath returns a filesystem path safe to use for a
// Unix domain socket from a container terminal integration test. The path is
// placed under testutil.TestTempRoot to stay within the AF_UNIX sun_path length
// limit on macOS and other Unix platforms.
func pickContainerTerminalSocketPath(t *testing.T) string {
	t.Helper()
	suffix, err := usvc_random.MakeRandomString(8)
	require.NoError(t, err)
	root := testutil.TestTempRoot()
	path := filepath.Join(root, fmt.Sprintf("dcp-ctr-term-%s.sock", suffix))
	t.Cleanup(func() {
		_ = os.Remove(path)
	})
	return path
}

// makeTerminalContainer returns a minimal Container with Spec.Terminal
// configured for the supplied UDS path. ContainerName is always set so the
// ContainerAttachFactoryDispatcher has a stable per-test routing key.
func makeTerminalContainer(name, udsPath string) *apiv1.Container {
	return &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: name,
			Image:         name + "-image",
			Terminal: &apiv1.TerminalSpec{
				UDSPath: udsPath,
				Cols:    120,
				Rows:    40,
			},
		},
	}
}

// ====================================================================================
// TestContainerOrchestrator-backed tests
// ====================================================================================

// TestContainerTerminalServerListensOnConfiguredSocket verifies that the container
// controller stands up a termpty.ConnManager on the configured UDS path and that
// the Hello frame reports version 1.
func TestContainerTerminalServerListensOnConfiguredSocket(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const ctrName = "test-ctr-term-listen"
	socketPath := pickContainerTerminalSocketPath(t)
	ctr := makeTerminalContainer(ctrName, socketPath)

	_ = containerAttachFactoryDispatcher.InstallHandler(
		t, ctr.Spec.ContainerName,
		ctrl_testutil.NewTestPtyContainerAttachFactory(testProcessExecutor))

	require.NoError(t, client.Create(ctx, ctr), "create Container")
	t.Cleanup(func() { _ = client.Delete(context.Background(), ctr) })

	_, _ = ensureContainerRunning(t, ctx, ctr)

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()

	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)
	// NOTE: ConnManager currently reports the Hmp1Server's default Hello
	// dimensions (80, 24) rather than the dimensions configured in
	// Spec.Terminal. That's an existing ConnManager limitation, not
	// something this test should pin down — we only assert the version here.
}

// TestContainerTerminalBridgesIO verifies that bytes written into the TestPty
// flow out as Hmp1 Output frames, and that Hmp1 Input frames written to the
// socket flow into the TestPty's outbound channel.
func TestContainerTerminalBridgesIO(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const ctrName = "test-ctr-term-io"
	socketPath := pickContainerTerminalSocketPath(t)
	ctr := makeTerminalContainer(ctrName, socketPath)

	infoCh := containerAttachFactoryDispatcher.InstallHandler(
		t, ctr.Spec.ContainerName,
		ctrl_testutil.NewTestPtyContainerAttachFactory(testProcessExecutor))

	require.NoError(t, client.Create(ctx, ctr), "create Container")
	t.Cleanup(func() { _ = client.Delete(context.Background(), ctr) })

	_, _ = ensureContainerRunning(t, ctx, ctr)
	var attached *ctrl_testutil.AttachedTerminalInfo
	select {
	case attached = <-infoCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty from container attach factory")
	}

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()
	_ = drainHelloAndStateSync(t, ctx, conn)

	// PTY -> Output frame.
	const ptyOut = "hello-from-container"
	select {
	case attached.TestPty.Inbound <- []byte(ptyOut):
	case <-ctx.Done():
		t.Fatal("timed out feeding TestPty inbound")
	}
	_ = readHmp1OutputAggregated(t, ctx, conn, ptyOut)

	// Input frame -> PTY.
	const clientIn = "hello-from-client"
	writeHmp1Frame(t, conn, termpty.Hmp1FrameInput, []byte(clientIn))
	select {
	case got := <-attached.TestPty.Outbound:
		require.Equal(t, clientIn, string(got), "expected PTY to receive bytes from Input frame")
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty to receive Input bytes")
	}
}

// TestContainerTerminalResizeFromClient verifies that Hmp1 Resize frames are
// applied to the PTY (recorded in TestPty.Resizes).
func TestContainerTerminalResizeFromClient(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const ctrName = "test-ctr-term-resize"
	socketPath := pickContainerTerminalSocketPath(t)
	ctr := makeTerminalContainer(ctrName, socketPath)

	infoCh := containerAttachFactoryDispatcher.InstallHandler(
		t, ctr.Spec.ContainerName,
		ctrl_testutil.NewTestPtyContainerAttachFactory(testProcessExecutor))

	require.NoError(t, client.Create(ctx, ctr), "create Container")
	t.Cleanup(func() { _ = client.Delete(context.Background(), ctr) })

	_, _ = ensureContainerRunning(t, ctx, ctr)
	var attached *ctrl_testutil.AttachedTerminalInfo
	select {
	case attached = <-infoCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty from container attach factory")
	}

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()
	_ = drainHelloAndStateSync(t, ctx, conn)

	resizePayload := make([]byte, 8)
	binary.LittleEndian.PutUint32(resizePayload[0:4], 132)
	binary.LittleEndian.PutUint32(resizePayload[4:8], 50)
	writeHmp1Frame(t, conn, termpty.Hmp1FrameResize, resizePayload)

	pollErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		resizes := attached.TestPty.ResizesSnapshot()
		for _, r := range resizes {
			if r.Cols == 132 && r.Rows == 50 {
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, pollErr, "expected TestPty to record a (132, 50) resize from client")
}

// TestContainerTerminalContainerExitSendsExitFrame verifies that
// when the underlying container exits, the connected client receives an Exit
// frame and the Container transitions to Exited.
func TestContainerTerminalContainerExitSendsExitFrame(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const ctrName = "test-ctr-term-exit"
	socketPath := pickContainerTerminalSocketPath(t)
	ctr := makeTerminalContainer(ctrName, socketPath)

	_ = containerAttachFactoryDispatcher.InstallHandler(
		t, ctr.Spec.ContainerName,
		ctrl_testutil.NewTestPtyContainerAttachFactory(testProcessExecutor))

	require.NoError(t, client.Create(ctx, ctr), "create Container")
	t.Cleanup(func() { _ = client.Delete(context.Background(), ctr) })

	updated, _ := ensureContainerRunning(t, ctx, ctr)

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()
	_ = drainHelloAndStateSync(t, ctx, conn)

	require.NoError(t,
		containerOrchestrator.SimulateContainerExit(ctx, updated.Status.ContainerID, 0),
		"could not simulate container exit")

	// Read frames until we see Exit. Don't pin down the exit code: the
	// production attach helper's exit code does not have to equal the
	// container's, and a forced PTY close (test path) can set its own value.
	for {
		ft, _ := readHmp1Frame(t, ctx, conn)
		if ft == termpty.Hmp1FrameExit {
			break
		}
	}

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateExited, nil
	})
}

// TestContainerTerminalDeletionTearsDownPty verifies that deleting the
// Container closes the TestPty.
func TestContainerTerminalDeletionTearsDownPty(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const ctrName = "test-ctr-term-delete"
	socketPath := pickContainerTerminalSocketPath(t)
	ctr := makeTerminalContainer(ctrName, socketPath)

	infoCh := containerAttachFactoryDispatcher.InstallHandler(
		t, ctr.Spec.ContainerName,
		ctrl_testutil.NewTestPtyContainerAttachFactory(testProcessExecutor))

	require.NoError(t, client.Create(ctx, ctr), "create Container")

	_, _ = ensureContainerRunning(t, ctx, ctr)
	var attached *ctrl_testutil.AttachedTerminalInfo
	select {
	case attached = <-infoCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty from container attach factory")
	}

	// Status should report the configured terminal socket path.
	running := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.TerminalSocketPath != "", nil
	})
	require.Equal(t, socketPath, running.Status.TerminalSocketPath, "Status should report the configured terminal socket path")

	// Sanity check: the socket file exists before deletion.
	_, statErr := os.Stat(socketPath)
	require.NoError(t, statErr, "socket file should exist before deletion")

	require.NoError(t, client.Delete(ctx, ctr), "delete Container")

	pollErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		return attached.TestPty.IsClosed(), nil
	})
	require.NoError(t, pollErr, "expected TestPty to be closed after Container deletion")

	// DCP owns the listen-mode socket file, so it must be removed once the terminal is torn down.
	removalErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		_, pollStatErr := os.Stat(socketPath)
		return errors.Is(pollStatErr, os.ErrNotExist), nil
	})
	require.NoError(t, removalErr, "terminal socket file should be removed after Container deletion")
}

// TestContainerTerminalGeneratesSocketPathWhenUDSPathEmpty verifies that a terminal
// Container created with an empty UDSPath reaches Running with a DCP-generated socket
// path reported in Status, that the socket exists and serves while Running, and that the
// generated file is removed once the Container is deleted.
func TestContainerTerminalGeneratesSocketPathWhenUDSPathEmpty(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const ctrName = "test-ctr-term-autopath"
	ctr := makeTerminalContainer(ctrName, "") // empty UDSPath: DCP generates the socket path.

	infoCh := containerAttachFactoryDispatcher.InstallHandler(
		t, ctr.Spec.ContainerName,
		ctrl_testutil.NewTestPtyContainerAttachFactory(testProcessExecutor))

	require.NoError(t, client.Create(ctx, ctr), "create Container")

	_, _ = ensureContainerRunning(t, ctx, ctr)
	var attached *ctrl_testutil.AttachedTerminalInfo
	select {
	case attached = <-infoCh:
	case <-ctx.Done():
		t.Fatal("timed out waiting for TestPty from container attach factory")
	}

	running := waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.TerminalSocketPath != "", nil
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

	require.NoError(t, client.Delete(ctx, ctr), "delete Container")

	pollErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		return attached.TestPty.IsClosed(), nil
	})
	require.NoError(t, pollErr, "expected TestPty to be closed after Container deletion")

	removalErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		_, pollStatErr := os.Stat(generatedPath)
		return errors.Is(pollStatErr, os.ErrNotExist), nil
	})
	require.NoError(t, removalErr, "generated terminal socket file should be removed after Container deletion")
}

// Verifies that when the container attach factory returns an error,
// the Container reaches FailedToStart state.
func TestContainerTerminalAttachFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, defaultIntegrationTestTimeout)
	defer cancel()

	const ctrName = "test-ctr-term-attach-fail"
	socketPath := pickContainerTerminalSocketPath(t)
	ctr := makeTerminalContainer(ctrName, socketPath)

	factoryErr := errors.New("simulated container attach failure")
	_ = containerAttachFactoryDispatcher.InstallHandler(
		t, ctr.Spec.ContainerName,
		func(_ context.Context, _ string, _ containers.AttachContainerOptions) (*termpty.PseudoTerminalProcess, error) {
			return nil, factoryErr
		})

	require.NoError(t, client.Create(ctx, ctr), "create Container")
	t.Cleanup(func() { _ = client.Delete(context.Background(), ctr) })

	waitObjectAssumesState(t, ctx, ctrl_client.ObjectKeyFromObject(ctr), func(c *apiv1.Container) (bool, error) {
		return c.Status.State == apiv1.ContainerStateFailedToStart, nil
	})
}

// ====================================================================================
// Real-orchestrator end-to-end test
// ====================================================================================

// TestContainerTerminalEndToEndWithRealOrchestrator exercises the full container
// terminal path through Docker/Podman attach to a real container running an
// interactive busybox shell. It is gated by SkipIfTrueContainerOrchestratorNotEnabled
// (i.e., DCP_TEST_ENABLE_TRUE_CONTAINER_ORCHESTRATOR=true).
func TestContainerTerminalEndToEndWithRealOrchestrator(t *testing.T) {
	testutil.SkipIfTrueContainerOrchestratorNotEnabled(t)

	t.Parallel()

	const testTimeout = 3 * time.Minute
	ctx, cancel := testutil.GetTestContext(t, testTimeout)
	defer cancel()

	serverInfo, teInfo, startupErr := StartAdvancedTestEnvironment(
		ctx,
		ContainerController,
		t.Name(),
		NoSeparateWorkingDir,
	)
	require.NoError(t, startupErr, "failed to start advanced test environment")
	defer teInfo.ProcessExecutor.Dispose()
	defer shutdownAdvancedTestEnvironment(t, ctx, cancel, serverInfo)

	apiClient := serverInfo.Client
	socketPath := pickContainerTerminalSocketPath(t)
	const ctrName = "test-ctr-term-e2e"

	ctr := &apiv1.Container{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ctrName,
			Namespace: metav1.NamespaceNone,
		},
		Spec: apiv1.ContainerSpec{
			ContainerName: ctrName,
			Image:         "busybox:latest",
			Command:       "sh",
			Args:          []string{"-i"},
			Terminal: &apiv1.TerminalSpec{
				UDSPath: socketPath,
				Cols:    120,
				Rows:    40,
			},
		},
	}

	require.NoError(t, apiClient.Create(ctx, ctr), "create Container")
	t.Cleanup(func() { _ = apiClient.Delete(context.Background(), ctr) })

	_ = waitObjectAssumesStateEx(t, ctx, apiClient, ctrl_client.ObjectKeyFromObject(ctr), func(currentCtr *apiv1.Container) (bool, error) {
		if currentCtr.Status.State == apiv1.ContainerStateFailedToStart {
			return false, fmt.Errorf("container failed to start: %s", currentCtr.Status.Message)
		}
		return currentCtr.Status.State == apiv1.ContainerStateRunning, nil
	})

	conn := dialTerminalSocketWhenReady(t, ctx, socketPath)
	defer conn.Close()

	hello := drainHelloAndStateSync(t, ctx, conn)
	require.Equal(t, 1, hello.Version)

	// Drive an echo round-trip through the shell. Use a unique marker so
	// it does not collide with the prompt or other noise the shell writes.
	const marker = "DCP_CONTAINER_TERMINAL_OK"
	writeHmp1Frame(t, conn, termpty.Hmp1FrameInput, []byte("echo "+marker+"\n"))
	aggregated := readHmp1OutputAggregated(t, ctx, conn, marker)
	require.True(t, strings.Contains(aggregated, marker),
		"expected aggregated output to contain marker %q", marker)

	// Delete the Container, then expect an Exit frame. Once the container
	// exits, the controller removes the finalizer, so the Container
	// resource is itself deleted — there is no final "Exited" state we
	// can observe via a GET. The Exit frame is our end-to-end confirmation
	// that the terminal lifecycle was torn down correctly.
	require.NoError(t, apiClient.Delete(ctx, ctr), "delete Container")

	for {
		ft, _ := readHmp1Frame(t, ctx, conn)
		if ft == termpty.Hmp1FrameExit {
			break
		}
	}
}
