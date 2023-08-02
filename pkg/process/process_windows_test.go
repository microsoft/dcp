//go:build windows

package process

import (
	"context"
	"syscall"
	"time"

	wait "k8s.io/apimachinery/pkg/util/wait"

	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

const (
	// https://learn.microsoft.com/en-us/windows/win32/procthread/process-security-and-access-rights
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000

	// https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess
	STILL_ACTIVE = 259
)

func ensureAllStopped(t *testing.T, pids []int32, timeout time.Duration) {
	timeoutCtx, timeoutCtxCancelFn := context.WithTimeout(context.Background(), timeout)
	defer timeoutCtxCancelFn()

	err := wait.PollUntilContextCancel(
		timeoutCtx,
		100*time.Millisecond,
		true, // Don't wait before polling for the first time
		func(_ context.Context) (bool, error) {
			noStopped := slices.LenIf(pids, isStopped)
			return noStopped == len(pids), nil
		},
	)

	require.NoError(t, err, "not all processes could be stopped")
}

func isStopped(pid int32) bool {
	handle, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
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
