/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/process"
)

// Verifies that MonitorPid records an initial process resolution failure as the monitor context's cancellation cause.
func TestMonitorPidRecordsInitialFailureCause(t *testing.T) {
	monitorCtx, monitorCancel, monitorErr := MonitorPid(
		context.Background(),
		process.NewHandle(process.UnknownPID, time.Time{}),
		0,
		logr.Discard(),
	)
	defer monitorCancel()
	require.Error(t, monitorErr)
	require.ErrorIs(t, context.Cause(monitorCtx), monitorErr)
}

// Verifies that MonitorPid retains ordinary context cancellation as the cancellation cause when monitoring is stopped.
func TestMonitorPidRecordsParentCancellation(t *testing.T) {
	currentHandle, handleErr := process.FindProcessHandle(process.Pid_t(os.Getpid()))
	require.NoError(t, handleErr)
	parentCtx, parentCancel := context.WithCancel(context.Background())
	monitorCtx, monitorCancel, monitorErr := MonitorPid(parentCtx, currentHandle, 0, logr.Discard())
	require.NoError(t, monitorErr)
	parentCancel()
	<-monitorCtx.Done()
	monitorCancel()
	require.ErrorIs(t, context.Cause(monitorCtx), context.Canceled)
}
