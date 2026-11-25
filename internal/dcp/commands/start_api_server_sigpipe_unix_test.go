//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	cmds "github.com/microsoft/dcp/internal/commands"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/statestore"
	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/internal/testutil/ctrlutil"
	"github.com/microsoft/dcp/pkg/commonapi"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	apiServerStartedMarker   = "API server started"
	controllersStartedMarker = "Starting workers"
)

type closedPipes uint32

const (
	closedPipesNone   closedPipes = 0
	closedPipesStdout closedPipes = 1
	closedPipesStderr closedPipes = 2
	closedPipesBoth               = closedPipesStdout | closedPipesStderr
)

// drainedPipe collects everything written to the read end of a pipe until the read end is closed.
type drainedPipe struct {
	reader  *os.File
	lock    sync.Mutex
	content bytes.Buffer
	done    chan struct{}
}

func newDrainedPipe(reader *os.File) *drainedPipe {
	dp := &drainedPipe{reader: reader, done: make(chan struct{})}
	go func() {
		defer close(dp.done)
		buf := make([]byte, 32*1024)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				dp.lock.Lock()
				dp.content.Write(buf[:n])
				dp.lock.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()
	return dp
}

func (dp *drainedPipe) String() string {
	dp.lock.Lock()
	defer dp.lock.Unlock()
	return dp.content.String()
}

// Closes the read end of the pipe so that subsequent writes to the pipe fail with EPIPE (or raise SIGPIPE).
func (dp *drainedPipe) closeReader(t *testing.T) {
	t.Helper()
	require.NoError(t, dp.reader.Close())
	<-dp.done
}

type apiServerProcess struct {
	command       *exec.Cmd
	stdout        *drainedPipe
	stderr        *drainedPipe
	exited        chan struct{}
	exitErr       error
	sessionFolder string
}

// Verifies that start-apiserver removes its session folder and exits without being killed by SIGPIPE
// when its stdout and/or stderr reader goes away before the monitored parent process exits.
func TestStartApiServerCleansUpSessionWhenOutputPipesAreClosed(t *testing.T) {
	testCases := []struct {
		name       string
		serverOnly bool
		closed     closedPipes
	}{
		{"server-only, output pipes open", true, closedPipesNone},
		{"server-only, stdout closed", true, closedPipesStdout},
		{"server-only, stderr closed", true, closedPipesStderr},
		{"server-only, stdout and stderr closed", true, closedPipesBoth},
		{"full, output pipes open", false, closedPipesNone},
		{"full, stdout closed", false, closedPipesStdout},
		{"full, stderr closed", false, closedPipesStderr},
		{"full, stdout and stderr closed", false, closedPipesBoth},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testCtx, testCancel := testutil.GetTestContext(t, 0)
			defer testCancel()

			owner := startOwnerProcess(t, testCtx)
			apiServer := startApiServerProcess(t, testCtx, tc.serverOnly, owner, "--monitor-interval", "1")

			waitForOutput(t, testCtx, apiServer, apiServerStartedMarker)
			if !tc.serverOnly {
				waitForOutput(t, testCtx, apiServer, controllersStartedMarker)
			}

			if tc.closed&closedPipesStdout != 0 {
				apiServer.stdout.closeReader(t)
			}
			if tc.closed&closedPipesStderr != 0 {
				apiServer.stderr.closeReader(t)
			}
			require.NoError(t, owner.Process.Kill())

			requireExitedWithoutSignal(t, testCtx, apiServer)
			require.NoDirExists(t, apiServer.sessionFolder)
		})
	}
}

// Verifies that start-apiserver removes its session folder when it fails during startup.
func TestStartApiServerCleansUpSessionOnEarlyError(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()

	owner := startOwnerProcess(t, testCtx)
	apiServer := startApiServerProcess(t, testCtx, true, owner,
		"--workload-id", strings.Repeat("a", commonapi.MaxWorkloadIDLength+1),
	)

	select {
	case <-apiServer.exited:
	case <-testCtx.Done():
		t.Fatalf("start-apiserver did not exit: %v; stderr: %s", testCtx.Err(), apiServer.stderr.String())
	}
	require.False(t, apiServer.command.ProcessState.Success(), apiServer.stderr.String())
	require.NoDirExists(t, apiServer.sessionFolder)
}

// Verifies that a command without SIGPIPE signal handling keeps the default behavior
// and is terminated by SIGPIPE when writing to a closed stdout pipe.
func TestCommandWithoutSigpipeSignalHandlingIsTerminatedBySigpipe(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()

	dcppaths.EnableTestPathProbing()
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr)
	stdoutRead, stdoutWrite, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	require.NoError(t, stdoutRead.Close())

	command := exec.CommandContext(testCtx, dcpPath, "version")
	command.Stdout = stdoutWrite
	require.NoError(t, command.Start())
	require.NoError(t, stdoutWrite.Close())

	runErr := command.Wait()
	var exitErr *exec.ExitError
	require.ErrorAs(t, runErr, &exitErr)
	waitStatus, isWaitStatus := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, isWaitStatus)
	require.True(t, waitStatus.Signaled(), "expected 'dcp version' to be terminated by a signal")
	require.Equal(t, syscall.SIGPIPE, waitStatus.Signal())
}

func startOwnerProcess(t *testing.T, ctx context.Context) *exec.Cmd {
	t.Helper()
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	command := exec.Command(delayPath, "--delay=3m")
	process.ForkFromParent(command)
	require.NoError(t, command.Start())
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-exited:
		case <-ctx.Done():
		}
	})
	return command
}

func startApiServerProcess(
	t *testing.T,
	ctx context.Context,
	serverOnly bool,
	owner *exec.Cmd,
	extraArgs ...string,
) *apiServerProcess {
	t.Helper()

	dcppaths.EnableTestPathProbing()
	dcpPath, dcpPathErr := dcppaths.GetDcpExePath()
	require.NoError(t, dcpPathErr)

	sessionFolder, sessionFolderErr := os.MkdirTemp(testutil.TestTempDir(), "dcp-sigpipe-session-")
	require.NoError(t, sessionFolderErr)
	t.Cleanup(func() { _ = os.RemoveAll(sessionFolder) })
	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")

	args := []string{
		"-v=debug",
		"start-apiserver",
		"--kubeconfig", kubeconfigPath,
		"--monitor", strconv.Itoa(owner.Process.Pid),
	}
	if serverOnly {
		tco, tcoErr := ctrlutil.NewTestContainerOrchestrator(
			context.WithoutCancel(ctx),
			logr.Discard(),
			ctrlutil.TcoOptionEnableSocketListener,
		)
		require.NoError(t, tcoErr)
		t.Cleanup(func() { _ = tco.Close() })
		args = append(args, "--server-only", "--test-container-orchestrator-socket", tco.GetSocketFilePath())
	}
	args = append(args, extraArgs...)

	stdoutRead, stdoutWrite, stdoutPipeErr := os.Pipe()
	require.NoError(t, stdoutPipeErr)
	stderrRead, stderrWrite, stderrPipeErr := os.Pipe()
	require.NoError(t, stderrPipeErr)

	// Do not use exec.CommandContext(): it kills the process on context cancellation, preventing cleanup.
	command := exec.Command(dcpPath, args...)
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, usvc_io.DCP_PRESERVE_EXECUTABLE_LOGS+"=") ||
			strings.HasPrefix(entry, cmds.DCPWorkloadIDEnvVar+"=")
	})
	command.Env = append(env,
		fmt.Sprintf("%s=%s", usvc_io.DCP_SESSION_FOLDER, sessionFolder),
		fmt.Sprintf("%s=%s", statestore.DCP_STATE_STORE_PATH, filepath.Join(sessionFolder, "state.sqlite3")),
	)
	command.Stdout = stdoutWrite
	command.Stderr = stderrWrite
	require.NoError(t, command.Start())
	require.NoError(t, stdoutWrite.Close())
	require.NoError(t, stderrWrite.Close())

	apiServer := &apiServerProcess{
		command:       command,
		stdout:        newDrainedPipe(stdoutRead),
		stderr:        newDrainedPipe(stderrRead),
		exited:        make(chan struct{}),
		sessionFolder: sessionFolder,
	}
	go func() {
		apiServer.exitErr = command.Wait()
		close(apiServer.exited)
	}()

	t.Cleanup(func() {
		select {
		case <-apiServer.exited:
		default:
			_ = command.Process.Kill()
			<-apiServer.exited
		}
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
		if t.Failed() {
			t.Logf("start-apiserver stdout:\n%s", apiServer.stdout.String())
			t.Logf("start-apiserver stderr:\n%s", apiServer.stderr.String())
		}
	})

	return apiServer
}

func waitForOutput(t *testing.T, ctx context.Context, apiServer *apiServerProcess, marker string) {
	t.Helper()
	pollErr := wait.PollUntilContextCancel(ctx, 50*time.Millisecond, true, func(context.Context) (bool, error) {
		select {
		case <-apiServer.exited:
			return false, fmt.Errorf("start-apiserver exited prematurely: %v", apiServer.exitErr)
		default:
		}
		return strings.Contains(apiServer.stderr.String(), marker), nil
	})
	require.NoError(t, pollErr, "waiting for start-apiserver output '%s'", marker)
}

func requireExitedWithoutSignal(t *testing.T, ctx context.Context, apiServer *apiServerProcess) {
	t.Helper()
	select {
	case <-apiServer.exited:
	case <-ctx.Done():
		t.Fatalf("start-apiserver did not exit: %v", ctx.Err())
	}
	exitErr := apiServer.exitErr

	var execExitErr *exec.ExitError
	if errors.As(exitErr, &execExitErr) {
		waitStatus, isWaitStatus := execExitErr.Sys().(syscall.WaitStatus)
		require.True(t, isWaitStatus)
		require.False(t, waitStatus.Signaled(), "start-apiserver was terminated by signal %v", waitStatus.Signal())
	}
	require.NoError(t, exitErr)
}
