/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestForkProcessStartsDetachedCommand(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer testCancel()

	forkedProcess := startForkedDelay(t, testCtx)

	require.False(t, forkedProcess.IdentityTime.IsZero(), "forked process start time should not be zero")
	checkExecutor := process.NewOSExecutor(testutil.NewLogForTesting(t.Name()))
	defer checkExecutor.Dispose()
	require.NoError(t, checkExecutor.CheckProcessRunning(forkedProcess))
}

func startForkedDelay(t *testing.T, testCtx context.Context) process.ProcessHandle {
	t.Helper()

	dcpProc, dcpProcErr := getDcpProcExecutablePath()
	require.NoError(t, dcpProcErr)

	delayToolPath, toolPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, toolPathErr)

	dcpProcCmd := exec.CommandContext(testCtx, dcpProc, "fork-process", delayToolPath, "--delay=30s")
	var stdout, stderr bytes.Buffer
	dcpProcCmd.Stdout = &stdout
	dcpProcCmd.Stderr = &stderr

	runErr := dcpProcCmd.Run()
	require.NoError(t, runErr, "dcp fork-process should exit cleanly; stderr: %s", stderr.String())

	stdoutText := strings.TrimSpace(stdout.String())
	childPid, parseErr := strconv.ParseInt(stdoutText, 10, 64)
	require.NoError(t, parseErr, "dcp fork-process should print only a child PID, got %q", stdoutText)

	pid := process.Pid_t(childPid)
	var identityTime time.Time
	cleanupExecutor := process.NewOSExecutor(testutil.NewLogForTesting(t.Name()))
	t.Cleanup(func() {
		_ = cleanupExecutor.StopProcess(process.NewHandle(pid, identityTime))
		cleanupExecutor.Dispose()
	})

	identityTime = process.ProcessIdentityTime(pid)
	require.False(t, identityTime.IsZero(), "forked process %d should still be running", pid)

	return process.NewHandle(pid, identityTime)
}
