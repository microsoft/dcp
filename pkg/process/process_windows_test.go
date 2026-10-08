//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	wait "k8s.io/apimachinery/pkg/util/wait"

	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	// https://learn.microsoft.com/en-us/windows/win32/procthread/process-security-and-access-rights
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000

	// https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess
	STILL_ACTIVE = 259
)

// Verifies that delay's forked child breaks away from the parent's Windows job
// and remains running after closing that job terminates the parent.
func TestForkFromParentBreaksAwayFromCurrentJob(t *testing.T) {
	t.Parallel()
	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	jobObject, jobCreationErr := windows.CreateJobObject(nil, nil)
	require.NoError(t, jobCreationErr)
	defer func() {
		if jobObject != windows.InvalidHandle {
			require.NoError(t, windows.CloseHandle(jobObject))
		}
	}()

	jobInformation := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, setJobInformationErr := windows.SetInformationJobObject(
		jobObject,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&jobInformation)),
		uint32(unsafe.Sizeof(jobInformation)),
	)
	require.NoError(t, setJobInformationErr)

	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	cmd := exec.Command(delayPath, "--delay=60s", "--child-spec=1", "--fork-children")
	commandLine, commandLineErr := windows.UTF16PtrFromString(windows.ComposeCommandLine(cmd.Args))
	require.NoError(t, commandLineErr)
	startupInfo := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var processInfo windows.ProcessInformation
	// Assign the job before delay can spawn a child.
	createErr := windows.CreateProcess(nil, commandLine, nil, nil, false,
		windows.CREATE_SUSPENDED|windows.CREATE_NO_WINDOW, nil, nil, &startupInfo, &processInfo)
	require.NoError(t, createErr)
	defer func() {
		exitResult, exitErr := windows.WaitForSingleObject(processInfo.Process, 0)
		require.NoError(t, exitErr)
		if exitResult == uint32(windows.WAIT_TIMEOUT) {
			require.NoError(t, windows.TerminateProcess(processInfo.Process, 1))
		}
		require.NoError(t, windows.CloseHandle(processInfo.Thread))
		require.NoError(t, windows.CloseHandle(processInfo.Process))
	}()

	assignJobErr := windows.AssignProcessToJobObject(jobObject, processInfo.Process)
	require.NoError(t, assignJobErr)
	root, rootErr := process.ProcessHandleFromNativeHandle(processInfo.Process)
	require.NoError(t, rootErr)
	_, resumeErr := windows.ResumeThread(processInfo.Thread)
	require.NoError(t, resumeErr)

	var childIdentity process.ProcessHandle
	var childProcess *os.Process
	childFoundErr := wait.PollUntilContextCancel(testCtx, 25*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		tree, snapshotErr := process.GetProcessTree(ctx, root)
		if snapshotErr != nil {
			return false, snapshotErr
		}
		for _, candidateIdentity := range tree[1:] {
			candidateProcess, candidateErr := candidateIdentity.OsProcess()
			if process.IsProcessGoneErr(candidateErr) {
				continue
			}
			if candidateErr != nil {
				return false, candidateErr
			}
			imagePath := make([]uint16, 32768)
			imagePathLength := uint32(len(imagePath))
			var imageErr error
			handleErr := candidateProcess.WithHandle(func(nativeHandle uintptr) {
				imageErr = windows.QueryFullProcessImageName(windows.Handle(nativeHandle), 0, &imagePath[0], &imagePathLength)
			})
			if inspectionErr := errors.Join(handleErr, imageErr); inspectionErr != nil {
				return false, fmt.Errorf("inspect descendant %d: %w", candidateIdentity.Pid, errors.Join(inspectionErr, candidateProcess.Release()))
			}
			imageName := filepath.Base(windows.UTF16ToString(imagePath[:imagePathLength]))
			if strings.EqualFold(imageName, filepath.Base(delayPath)) {
				childIdentity = candidateIdentity
				childProcess = candidateProcess
				return true, nil
			}
			if releaseErr := candidateProcess.Release(); releaseErr != nil {
				return false, releaseErr
			}
		}
		return false, nil
	})
	require.NoError(t, childFoundErr, "forked delay child did not appear in the process tree")
	defer func() {
		if !isStopped(childIdentity) {
			killErr := childProcess.Kill()
			require.True(t, killErr == nil || process.IsProcessGoneErr(killErr), "could not clean up delay child: %v", killErr)
		}
		require.NoError(t, childProcess.Release())
	}()

	require.NoError(t, windows.CloseHandle(jobObject))
	jobObject = windows.InvalidHandle
	rootExitErr := wait.PollUntilContextCancel(testCtx, 25*time.Millisecond, true, func(context.Context) (bool, error) {
		waitResult, waitErr := windows.WaitForSingleObject(processInfo.Process, 0)
		return waitResult == windows.WAIT_OBJECT_0, waitErr
	})
	require.NoError(t, rootExitErr)
	require.True(t, isStopped(root))
	require.False(t, isStopped(childIdentity), "forked delay child must outlive termination of the parent's job")
}

func ensureAllStopped(t *testing.T, processes []process.ProcessHandle, timeout time.Duration) {
	timeoutCtx, timeoutCtxCancelFn := context.WithTimeout(context.Background(), timeout)
	defer timeoutCtxCancelFn()

	err := wait.PollUntilContextCancel(
		timeoutCtx,
		100*time.Millisecond,
		true, // Don't wait before polling for the first time
		func(_ context.Context) (bool, error) {
			noStopped := slices.LenIf(processes, isStopped)
			return noStopped == len(processes), nil
		},
	)

	require.NoError(t, err, "not all processes could be stopped")
}

func isStopped(pp process.ProcessHandle) bool {
	osPid, err := process.PidT_ToUint32(pp.Pid)
	if err != nil {
		// Invalid PID value, so there is no process with such ID
		return true
	}

	handle, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, osPid)
	if err != nil {
		return true // Process not found, assume it's stopped
	}

	defer func() { _ = syscall.CloseHandle(handle) }()

	var exitCode uint32
	err = syscall.GetExitCodeProcess(handle, &exitCode)
	if err != nil {
		return false // Err on the side of saying "the process is still running"
	}

	if exitCode == STILL_ACTIVE {
		return false
	} else {
		return true
	}
}
