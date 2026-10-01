//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// Verifies that normal starts derive stop topology from the final Windows creation flags,
// after the executor has added its own process-group and cleanup-job flags.
func TestWindowsConsoleAvailabilityUsesFinalCreationFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		flags        uint32
		availability WindowsConsoleAvailability
	}{
		{
			name:         "inherited classic console",
			availability: WindowsConsoleAvailabilityInherited,
		},
		{
			name:         "new classic console",
			flags:        windows.CREATE_NEW_CONSOLE,
			availability: WindowsConsoleAvailabilityRequiresAttach,
		},
		{
			name:         "detached process",
			flags:        windows.DETACHED_PROCESS,
			availability: WindowsConsoleAvailabilityUnavailable,
		},
		{
			name:         "no console window",
			flags:        windows.CREATE_NO_WINDOW,
			availability: WindowsConsoleAvailabilityUnavailable,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
			defer executor.Dispose()
			command := exec.Command("unused")
			command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: testCase.flags}

			executor.prepareProcessStart(command, CreationFlagEnsureKillOnDispose)

			require.Equal(t, testCase.availability, windowsConsoleAvailabilityForCmd(command))
			require.NotZero(t, command.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP)
			require.NotZero(t, command.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED)
		})
	}
}

// Verifies which console event may be sent for each topology and that only a confirmed
// CTRL_C_EVENT or CTRL_BREAK_EVENT dispatch can consume the full graceful deadline.
func TestWindowsConsoleControlStopPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		winConsoleAvailability WindowsConsoleAvailability
		opts                   processStoppingOpts
		event                  uint32
		processGroupID         uint32
		shouldSend             bool
	}{
		{
			name:                   "attached helper sends control c to console",
			winConsoleAvailability: WindowsConsoleAvailabilityUnknown,
			opts:                   optTrySignal | optSignalConsoleGroup,
			event:                  windows.CTRL_C_EVENT,
			processGroupID:         0,
			shouldSend:             true,
		},
		{
			name:                   "inherited console sends control break to launch group",
			winConsoleAvailability: WindowsConsoleAvailabilityInherited,
			opts:                   optTrySignal,
			event:                  windows.CTRL_BREAK_EVENT,
			processGroupID:         4321,
			shouldSend:             true,
		},
		{
			name:                   "new console needs helper confirmation",
			winConsoleAvailability: WindowsConsoleAvailabilityRequiresAttach,
			opts:                   optTrySignal,
		},
		{
			name:                   "conpty or detached process has no classic console event",
			winConsoleAvailability: WindowsConsoleAvailabilityUnavailable,
			opts:                   optTrySignal,
		},
		{
			name:                   "unknown descendant is not treated as a process group",
			winConsoleAvailability: WindowsConsoleAvailabilityUnknown,
			opts:                   optTrySignal | optGracefulOnly,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			event, processGroupID, shouldSend := consoleControlEventForStop(
				testCase.winConsoleAvailability,
				testCase.opts,
				4321,
			)
			require.Equal(t, testCase.event, event)
			require.Equal(t, testCase.processGroupID, processGroupID)
			require.Equal(t, testCase.shouldSend, shouldSend)
		})
	}

	require.Zero(t, consoleControlWaitTimeout(true, optWaitForGracefulDeadline),
		"confirmed delivery uses the caller's 15-second graceful deadline")
	require.Zero(t, consoleControlWaitTimeout(true, optGracefulOnly),
		"confirmed descendant delivery uses the remaining graceful deadline")
	require.Equal(t, signalAndWaitTimeout, consoleControlWaitTimeout(false, optWaitForGracefulDeadline),
		"unconfirmed root delivery uses the six-second passive fallback")
	require.Equal(t, signalAndWaitTimeout, consoleControlWaitTimeout(false, optGracefulOnly),
		"unknown descendants use the six-second passive fallback")
}

// Verifies that StartAndForget retains the same minimal wait/console-availability state as a
// tracked start, so a later stop can use the launch topology without adding a second waiter.
func TestStartAndForgetRetainsWindowsConsoleAvailability(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()
	executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
	defer executor.Dispose()
	command := exec.Command(os.Args[0], "-test.run=^TestCleanupJobAssignmentTargetProcess$")
	command.Env = append(os.Environ(), cleanupJobAssignmentTargetEnvVar+"=1")

	handle, startErr := executor.StartAndForget(command, CreationFlagsNone)
	require.NoError(t, startErr)

	executor.acquireLock()
	state := executor.procsWaiting[handle]
	executor.releaseLock()
	require.NotNil(t, state)
	require.Equal(t, WindowsConsoleAvailabilityInherited, state.winConsoleAvailability)
	require.True(t, state.waitStarted)

	process, findErr := FindProcess(handle)
	require.NoError(t, findErr)
	require.NoError(t, process.Kill())
	require.NoError(t, process.Release())
	select {
	case <-state.waitEndedCh:
	case <-testCtx.Done():
		t.Fatal("StartAndForget process was not reaped")
	}
}

// Verifies that cleanup ownership failures abort and reap a suspended start instead of
// resuming a process which CreationFlagEnsureKillOnDispose cannot own.
func TestEnsureKillOnDisposeFailsClosed(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	tests := []struct {
		name      string
		configure func(*OSExecutor, error)
	}{
		{
			name: "cleanup job unavailable",
			configure: func(executor *OSExecutor, injectedErr error) {
				executor.processCleanupJob = func() windows.Handle {
					executor.processCleanupJobCreated = true
					executor.processCleanupJobErr = injectedErr
					return windows.InvalidHandle
				}
			},
		},
		{
			name: "process open failed",
			configure: func(executor *OSExecutor, injectedErr error) {
				executor.openProcessForCleanupJob = func(uint32, bool, uint32) (windows.Handle, error) {
					return windows.InvalidHandle, injectedErr
				}
			},
		},
		{
			name: "job assignment failed",
			configure: func(executor *OSExecutor, injectedErr error) {
				executor.assignProcessToCleanupJob = func(windows.Handle, windows.Handle) error {
					return injectedErr
				}
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			executor := NewOSExecutor(logr.Discard()).(*OSExecutor)
			defer executor.Dispose()
			injectedErr := errors.New("injected cleanup ownership failure")
			testCase.configure(executor, injectedErr)
			command := exec.Command(os.Args[0], "-test.run=^TestCleanupJobAssignmentTargetProcess$")
			command.Env = append(os.Environ(), cleanupJobAssignmentTargetEnvVar+"=1")

			handle, startWaiting, startErr := executor.StartProcess(
				testCtx,
				command,
				nil,
				CreationFlagEnsureKillOnDispose,
				nil,
			)

			require.Equal(t, ProcessHandle{Pid: UnknownPID}, handle)
			require.Nil(t, startWaiting)
			require.ErrorIs(t, startErr, injectedErr)
			require.NotNil(t, command.Process)
			require.NotNil(t, command.ProcessState)
			require.True(t, command.ProcessState.Exited(), "failed start must be killed and reaped")
		})
	}
}
