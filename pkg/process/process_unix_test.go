//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	wait "k8s.io/apimachinery/pkg/util/wait"

	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

// Tests that processes that ignore SIGTERM can still be terminated.
// Run on Unix-like systems only, because Windows does not have signals.
func TestStopProcessIgnoreSigterm(t *testing.T) {
	t.Parallel()

	delayToolDir, err := getDelayToolDir()
	require.NoError(t, err)

	const delay = 20 * time.Second
	cmd := exec.Command("./delay", fmt.Sprintf("--delay=%s", delay.String()), "--ignore-sigterm")
	cmd.Dir = delayToolDir

	err = cmd.Start()
	require.NoError(t, err, "could not start the 'delay' test program")
	defer func() {
		_ = cmd.Wait()
	}()

	pid, err := IntToPidT(cmd.Process.Pid)
	require.NoError(t, err)

	// Only one process should be running, so the "tree" size is 1.
	ensureProcessTree(t, pid, 1, 5*time.Second)

	executor := NewOSExecutor(log)
	start := time.Now()
	err = executor.StopProcess(pid)
	require.NoError(t, err)
	elapsed := time.Since(start)
	elapsedStr := testutil.FormatDuration(elapsed)
	if elapsed > delay {
		// It is expected that the process will not exit immediately, because it will ignore SIGTERM.
		// It should not take more than `signalAndWaitTimeout` though.
		t.Fatal("Process was not terminated timely, elapsed time was ", elapsedStr)
	}
	ensureAllStopped(t, []Pid_t{pid}, 5*time.Second)
}

func ensureAllStopped(t *testing.T, pids []Pid_t, timeout time.Duration) {
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

func isStopped(pid Pid_t) bool {
	// On Unix-like systems FindProcess() always succeeds, so it is not a reliable way of checking
	// if the process is still running.
	osPid, err := PidT_ToInt(pid)
	if err != nil {
		panic(err)
	}

	proc, findProcessErr := os.FindProcess(osPid)
	if findProcessErr != nil {
		return true
	}
	// The SIGWINCH (window resize) is ignored by default, so it is a good one to use
	// as a "Are you there?" query
	signalSendErr := proc.Signal(syscall.SIGWINCH)
	return errors.Is(signalSendErr, os.ErrProcessDone)
}
