/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package hosting

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	commandServiceTestName = "test-service"

	// The test executor hands out sequential PIDs starting at 1, and every test uses its own executor.
	firstTestProcessPID process.Pid_t = 1
)

// newTestCommandService creates a CommandService backed by an executor that never runs a real process.
func newTestCommandService(
	t *testing.T,
	executor process.Executor,
	options CommandServiceRunOptions,
) *CommandService {
	t.Helper()

	return NewCommandService(
		commandServiceTestName,
		exec.Command("dcp-command-service-test"),
		executor,
		options,
		logr.Discard(),
	)
}

// waitForTestProcessStart blocks until the service has started its process, so the test can
// simulate an exit for it.
func waitForTestProcessStart(t *testing.T, ctx context.Context, executor *testutil.TestProcessExecutor) {
	t.Helper()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, found := executor.FindByPid(firstTestProcessPID); found {
			return
		}
		select {
		case <-ctx.Done():
			require.FailNow(t, "timed out waiting for the service process to start")
		case <-ticker.C:
		}
	}
}

func runCommandServiceAsync(svc *CommandService, ctx context.Context) <-chan error {
	runResult := make(chan error, 1)
	go func() {
		runResult <- svc.Run(ctx)
	}()
	return runResult
}

func Test_CommandService_CleanExitIsNotAnError(t *testing.T) {
	execCtx, cancelExecCtx := context.WithDeadline(createContext(t), time.Now().Add(time.Second*10))
	defer cancelExecCtx()

	executor := testutil.NewTestProcessExecutor(execCtx)
	defer executor.Dispose()

	svc := newTestCommandService(t, executor, 0)
	runResult := runCommandServiceAsync(svc, execCtx)

	waitForTestProcessStart(t, execCtx, executor)
	executor.SimulateProcessExit(t, firstTestProcessPID, 0)

	require.NoError(t, <-runResult)
}

// Test_CommandService_UnexpectedExitIsReported covers the failure that leaves DCP without the
// component the service was hosting: the process exits on its own with a non-zero code while the
// service is still supposed to be running.
func Test_CommandService_UnexpectedExitIsReported(t *testing.T) {
	execCtx, cancelExecCtx := context.WithDeadline(createContext(t), time.Now().Add(time.Second*10))
	defer cancelExecCtx()

	executor := testutil.NewTestProcessExecutor(execCtx)
	defer executor.Dispose()

	svc := newTestCommandService(t, executor, 0)
	runResult := runCommandServiceAsync(svc, execCtx)

	waitForTestProcessStart(t, execCtx, executor)
	executor.SimulateProcessExit(t, firstTestProcessPID, 1)

	runErr := <-runResult
	require.Error(t, runErr)
	require.ErrorContains(t, runErr, commandServiceTestName)
	require.ErrorContains(t, runErr, "exit code 1")
}

// Test_CommandService_ExitAfterCancellationIsNotAnError verifies that shutting DCP down does not
// report a failure. Terminating a process yields a non-zero exit code on most platforms, and that
// is the expected outcome once the service context is cancelled.
func Test_CommandService_ExitAfterCancellationIsNotAnError(t *testing.T) {
	execCtx, cancelExecCtx := context.WithDeadline(createContext(t), time.Now().Add(time.Second*10))
	defer cancelExecCtx()

	executor := testutil.NewTestProcessExecutor(execCtx)
	defer executor.Dispose()

	svcCtx, cancelSvcCtx := context.WithCancel(execCtx)
	defer cancelSvcCtx()

	svc := newTestCommandService(t, executor, 0)
	runResult := runCommandServiceAsync(svc, svcCtx)

	waitForTestProcessStart(t, execCtx, executor)
	cancelSvcCtx()
	executor.SimulateProcessExit(t, firstTestProcessPID, testutil.KilledProcessExitCode)

	require.NoError(t, <-runResult)
}

// Test_CommandService_UnexpectedExitIsReportedWhenNotTerminated covers the same unexpected exit for
// services that outlive their context (CommandServiceRunOptionDontTerminate), which take a
// different path out of Run.
func Test_CommandService_UnexpectedExitIsReportedWhenNotTerminated(t *testing.T) {
	execCtx, cancelExecCtx := context.WithDeadline(createContext(t), time.Now().Add(time.Second*10))
	defer cancelExecCtx()

	executor := testutil.NewTestProcessExecutor(execCtx)
	defer executor.Dispose()

	svc := newTestCommandService(t, executor, CommandServiceRunOptionDontTerminate)
	runResult := runCommandServiceAsync(svc, execCtx)

	waitForTestProcessStart(t, execCtx, executor)
	executor.SimulateProcessExit(t, firstTestProcessPID, 3)

	runErr := <-runResult
	require.Error(t, runErr)
	require.ErrorContains(t, runErr, "exit code 3")
}

// Test_CommandService_ProcessTrackingErrorIsReported verifies that a failure to observe the process
// exit is surfaced even though no exit code is available.
func Test_CommandService_ProcessTrackingErrorIsReported(t *testing.T) {
	execCtx, cancelExecCtx := context.WithDeadline(createContext(t), time.Now().Add(time.Second*10))
	defer cancelExecCtx()

	trackingErr := errors.New("could not track the process")
	executor := &exitErrorExecutor{
		TestProcessExecutor: testutil.NewTestProcessExecutor(execCtx),
		exitErr:             trackingErr,
	}
	defer executor.Dispose()

	svc := newTestCommandService(t, executor, 0)

	require.ErrorIs(t, svc.Run(execCtx), trackingErr)
}

// exitErrorExecutor reports process exit with an error instead of an exit code, which happens when
// the process could not be tracked to completion.
type exitErrorExecutor struct {
	*testutil.TestProcessExecutor
	exitErr error
}

func (e *exitErrorExecutor) StartProcess(
	ctx context.Context,
	cmd *exec.Cmd,
	exitHandler process.ProcessExitHandler,
	creationFlags process.ProcessCreationFlag,
	sysCreateProcess process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	handle, startWaitForProcessExit, startErr := e.TestProcessExecutor.StartProcess(
		ctx,
		cmd,
		exitHandler,
		creationFlags,
		sysCreateProcess,
	)
	if startErr != nil {
		return handle, startWaitForProcessExit, startErr
	}

	return handle, func() {
		startWaitForProcessExit()
		exitHandler.OnProcessExited(handle.Pid, process.UnknownExitCode, e.exitErr)
	}, nil
}

var _ process.Executor = (*exitErrorExecutor)(nil)
