//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package wslc

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type recordingProcessExecutor struct {
	process.Executor
	started chan process.ProcessHandle
}

func (executor *recordingProcessExecutor) StartProcess(
	ctx context.Context,
	command *exec.Cmd,
	handler process.ProcessExitHandler,
	flags process.ProcessCreationFlag,
	create process.SysCreateProcessFunc,
) (process.ProcessHandle, func(), error) {
	handle, startWaiting, startErr := executor.Executor.StartProcess(ctx, command, handler, flags, create)
	if startErr == nil {
		executor.started <- handle
	}
	return handle, startWaiting, startErr
}

// Verifies that buffered commands delegate cancellation to the shared executor, which gracefully stops and reaps a delay tree in the inherited console.
func TestWslcCommandCancellationUsesExecutor(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := process.NewOSExecutor(testr.New(t))
	defer executor.Dispose()
	observedExecutor := &recordingProcessExecutor{
		Executor: executor,
		started:  make(chan process.ProcessHandle, 1),
	}
	orchestrator := NewWslcCliOrchestrator(testr.New(t), observedExecutor).(*WslcCliOrchestrator)

	delayPath, delayPathErr := internal_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	command := exec.Command(delayPath, "--delay=3m", "--child-spec=1", "--couple-children")
	commandCtx, commandCancel := context.WithCancel(testCtx)
	defer commandCancel()
	result := make(chan error, 1)
	commandDone := make(chan struct{})
	go func() {
		defer close(commandDone)
		_, _, runErr := orchestrator.runBufferedWslcCommand(
			commandCtx, "CancellationTest", command, nil, nil, time.Minute,
		)
		result <- runErr
	}()
	defer func() {
		commandCancel()
		<-commandDone
	}()
	var handle process.ProcessHandle
	select {
	case startedHandle, open := <-observedExecutor.started:
		require.True(t, open, "process starts closed before the delay process was observed")
		handle = startedHandle
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}

	// Delay installs its signal handler before spawning the child in the same process group.
	readyErr := wait.PollUntilContextCancel(testCtx, 10*time.Millisecond, true, func(pollCtx context.Context) (bool, error) {
		tree, treeErr := process.GetProcessTree(pollCtx, handle)
		return len(tree) >= 2, treeErr
	})
	require.NoError(t, readyErr)

	startedAt := time.Now()
	commandCancel()
	select {
	case runErr := <-result:
		require.ErrorIs(t, runErr, context.Canceled)
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	require.Less(t, time.Since(startedAt), 5500*time.Millisecond)
	require.NotNil(t, command.ProcessState)
	require.Zero(t, command.ProcessState.ExitCode(), "delay should exit on the console interrupt, not be force-killed")
}
