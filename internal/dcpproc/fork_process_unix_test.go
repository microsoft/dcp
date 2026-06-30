//go:build !windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dcpproc_test

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/process"
)

func TestForkProcessStartsOutsideParentSession(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer testCancel()

	forkedProcess := startForkedDelay(t, testCtx)

	childPid, childPidErr := process.PidT_ToInt(forkedProcess.Pid)
	require.NoError(t, childPidErr)

	childProcessGroup, childProcessGroupErr := syscall.Getpgid(childPid)
	require.NoError(t, childProcessGroupErr)

	currentProcessGroup, currentProcessGroupErr := syscall.Getpgid(0)
	require.NoError(t, currentProcessGroupErr)

	childSession, childSessionErr := syscall.Getsid(childPid)
	require.NoError(t, childSessionErr)

	currentSession, currentSessionErr := syscall.Getsid(0)
	require.NoError(t, currentSessionErr)

	require.Equal(t, childPid, childProcessGroup, "forked process should lead its own process group")
	require.NotEqual(t, currentProcessGroup, childProcessGroup, "forked process should be outside the parent's process group")
	require.Equal(t, childPid, childSession, "forked process should lead its own session")
	require.NotEqual(t, currentSession, childSession, "forked process should be outside the parent's session")
}
