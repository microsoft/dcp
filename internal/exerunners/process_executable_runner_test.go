/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/dcppaths"
	"github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

func TestProcessExecutableRunnerSkipsMonitorForPersistentExecutable(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name                  string
		persistent            bool
		expectedMonitorStarts int
	}{
		{
			name:                  "non-persistent executable starts monitor",
			expectedMonitorStarts: 1,
		},
		{
			name:       "persistent executable skips monitor",
			persistent: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dcppaths.EnableTestPathProbing()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			processExecutor := testutil.NewTestProcessExecutor(ctx)
			runner := NewProcessExecutableRunner(processExecutor)
			exe := &apiv1.Executable{
				Spec: apiv1.ExecutableSpec{
					ExecutablePath: "/test/app",
					Persistent:     testCase.persistent,
				},
			}

			result := runner.StartRun(ctx, exe, &testRunChangeHandler{}, logr.Discard())

			require.Equal(t, apiv1.ExecutableStateRunning, result.ExeState)
			require.Len(t, processExecutor.FindAll([]string{"/test/app"}, "", nil), 1)
			require.Len(t, processExecutor.FindAll([]string{"dcp", "monitor-process"}, "", nil), testCase.expectedMonitorStarts)
		})
	}
}

func TestAdoptedProcessStopUsesAdoptedPID(t *testing.T) {
	t.Parallel()

	dcppaths.EnableTestPathProbing()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	processExecutor := &recordingProcessExecutor{}
	runner := NewProcessExecutableRunner(processExecutor)
	pid := process.Pid_t(42)
	identityTime := time.Now().UTC()
	originalRunID := pidToRunID(pid)
	adoptedRunID := controllers.RunID(fmt.Sprintf("%s-adopted", originalRunID))
	runner.runningProcesses.Store(adoptedRunID, &processRunState{
		pid:          pid,
		identityTime: identityTime,
		cmdInfo:      "/test/app",
		adopted:      true,
	})

	require.NoError(t, runner.StopRun(ctx, adoptedRunID, logr.Discard()))

	require.Equal(t, pid, processExecutor.stoppedPID)
	require.Equal(t, identityTime, processExecutor.stoppedIdentityTime)
}

type recordingProcessExecutor struct {
	stoppedPID          process.Pid_t
	stoppedIdentityTime time.Time
}

func (e *recordingProcessExecutor) StartProcess(context.Context, *exec.Cmd, process.ProcessExitHandler, process.ProcessCreationFlag) (process.Pid_t, time.Time, func(), error) {
	return process.UnknownPID, time.Time{}, nil, fmt.Errorf("not implemented")
}

func (e *recordingProcessExecutor) StopProcess(pid process.Pid_t, processStartTime time.Time, _ ...process.ProcessStopOption) error {
	e.stoppedPID = pid
	e.stoppedIdentityTime = processStartTime
	return nil
}

func (e *recordingProcessExecutor) StartAndForget(*exec.Cmd, process.ProcessCreationFlag) (process.Pid_t, time.Time, error) {
	return process.UnknownPID, time.Time{}, fmt.Errorf("not implemented")
}

func (e *recordingProcessExecutor) Dispose() {}

type testRunChangeHandler struct{}

func (*testRunChangeHandler) OnMainProcessChanged(controllers.RunID, process.Pid_t) {}

func (*testRunChangeHandler) OnRunCompleted(controllers.RunID, *int32, error) {}

func (*testRunChangeHandler) OnStartupCompleted(types.NamespacedName, *controllers.ExecutableStartResult) {
}

func (*testRunChangeHandler) OnRunMessage(controllers.RunID, controllers.RunMessageLevel, string) {}
