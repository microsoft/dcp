//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	wait "k8s.io/apimachinery/pkg/util/wait"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	// https://learn.microsoft.com/en-us/windows/win32/procthread/process-security-and-access-rights
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000

	// https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess
	STILL_ACTIVE = 259

	forkFromParentHelperEnvVar = "DCP_TEST_FORK_FROM_PARENT_IN_BREAKAWAY_JOB"
)

func TestForkFromParentBreaksAwayFromCurrentJob(t *testing.T) {
	if os.Getenv(forkFromParentHelperEnvVar) == "1" {
		waitForJobAssignment := make([]byte, 1)
		_, readErr := io.ReadFull(os.Stdin, waitForJobAssignment)
		require.NoError(t, readErr)

		childCmd := exec.Command("unused")
		process.ForkFromParent(childCmd)

		require.NotNil(t, childCmd.SysProcAttr)
		require.NotZero(t, childCmd.SysProcAttr.CreationFlags&windows.CREATE_BREAKAWAY_FROM_JOB)
		return
	}

	testCtx, testCancel := testutil.GetTestContext(t, 30*time.Second)
	defer testCancel()

	jobObject, jobCreationErr := windows.CreateJobObject(nil, nil)
	require.NoError(t, jobCreationErr)
	defer func() {
		_ = windows.CloseHandle(jobObject)
	}()

	jobInformation := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK,
		},
	}
	_, setJobInformationErr := windows.SetInformationJobObject(
		jobObject,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&jobInformation)),
		uint32(unsafe.Sizeof(jobInformation)),
	)
	require.NoError(t, setJobInformationErr)

	var helperOutput bytes.Buffer
	helperCmd := exec.CommandContext(testCtx, os.Args[0], "-test.run=^TestForkFromParentBreaksAwayFromCurrentJob$")
	helperCmd.Env = append(os.Environ(), forkFromParentHelperEnvVar+"=1")
	helperCmd.Stdout = &helperOutput
	helperCmd.Stderr = &helperOutput

	helperStdin, stdinPipeErr := helperCmd.StdinPipe()
	require.NoError(t, stdinPipeErr)
	defer func() {
		_ = helperStdin.Close()
	}()

	helperStartErr := helperCmd.Start()
	require.NoError(t, helperStartErr)

	helperExited := false
	defer func() {
		if !helperExited {
			_ = helperCmd.Process.Kill()
			_ = helperCmd.Wait()
		}
	}()

	helperProcessHandle, openProcessErr := windows.OpenProcess(windows.PROCESS_ALL_ACCESS, false, uint32(helperCmd.Process.Pid))
	require.NoError(t, openProcessErr)
	defer func() {
		_ = windows.CloseHandle(helperProcessHandle)
	}()

	assignJobErr := windows.AssignProcessToJobObject(jobObject, helperProcessHandle)
	require.NoError(t, assignJobErr)

	_, signalHelperErr := helperStdin.Write([]byte{1})
	require.NoError(t, signalHelperErr)

	closeStdinErr := helperStdin.Close()
	require.NoError(t, closeStdinErr)

	helperWaitErr := helperCmd.Wait()
	helperExited = true
	require.NoErrorf(t, helperWaitErr, "helper process failed:\n%s", helperOutput.String())
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
