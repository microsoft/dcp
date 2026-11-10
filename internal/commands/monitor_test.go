/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	int_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
)

type blockingMonitorLogSink struct {
	started chan struct{}
	release chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newBlockingMonitorLogSink() *blockingMonitorLogSink {
	return &blockingMonitorLogSink{
		started: make(chan struct{}),
		release: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (sink *blockingMonitorLogSink) Init(logr.RuntimeInfo) {
}

func (sink *blockingMonitorLogSink) Enabled(int) bool {
	return true
}

func (sink *blockingMonitorLogSink) Info(_ int, message string, _ ...any) {
	if message != "Monitor process exited, shutting down" {
		return
	}
	sink.once.Do(func() {
		close(sink.started)
		<-sink.release
		close(sink.done)
	})
}

func (sink *blockingMonitorLogSink) Error(error, string, ...any) {
}

func (sink *blockingMonitorLogSink) WithValues(...any) logr.LogSink {
	return sink
}

func (sink *blockingMonitorLogSink) WithName(string) logr.LogSink {
	return sink
}

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

// Verifies that MonitorPid publishes process completion before terminal logging, so a blocked
// diagnostic cannot delay monitor-context cancellation.
func TestMonitorPidCancelsBeforeTerminalLogging(t *testing.T) {
	testCtx, testCancel := testutil.GetTestContext(t, 0)
	defer testCancel()
	delayPath, delayPathErr := int_testutil.GetTestToolPath("delay")
	require.NoError(t, delayPathErr)
	command := exec.Command(delayPath, "--delay=500ms")
	process.ForkFromParent(command)
	require.NoError(t, command.Start())
	handle, handleErr := process.ProcessHandleFromCmd(command)
	require.NoError(t, handleErr)
	t.Cleanup(func() {
		executor := process.NewOSExecutor(logr.Discard())
		defer executor.Dispose()
		stopCtx, stopCancel := process.WithDetachedStopTimeout(testCtx)
		defer stopCancel()
		if executor.CheckProcessRunning(handle) == nil {
			_ = executor.StopProcess(stopCtx, handle, process.StopRootOnly())
		}
	})

	sink := newBlockingMonitorLogSink()
	monitorCtx, monitorCancel, monitorErr := MonitorPid(testCtx, handle, 0, logr.New(sink))
	require.NoError(t, monitorErr)
	defer monitorCancel()
	select {
	case <-sink.started:
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
	select {
	case <-monitorCtx.Done():
	default:
		t.Fatal("monitor context was not cancelled before terminal logging")
	}
	require.ErrorIs(t, context.Cause(monitorCtx), context.Canceled)
	close(sink.release)
	select {
	case <-sink.done:
	case <-testCtx.Done():
		t.Fatal(testCtx.Err())
	}
}

// Verifies that MonitorPidFromFlags monitors the process selected by the monitor flags, reports unusable
// --monitor values (including -1) as errors with an already cancelled context, and that GetMonitorContextFromFlags
// still treats -1 as having no process to monitor.
func TestMonitorPidFromFlags(t *testing.T) {
	savedPid, savedStartTime, savedInterval := monitorPid, monitorProcessStartTime, monitorInterval
	t.Cleanup(func() {
		monitorPid, monitorProcessStartTime, monitorInterval = savedPid, savedStartTime, savedInterval
	})
	currentHandle, handleErr := process.FindProcessHandle(process.Pid_t(os.Getpid()))
	require.NoError(t, handleErr)
	identityTime := currentHandle.IdentityTime.Format(osutil.RFC3339MiliTimestampFormat)

	parseMonitorFlags(t, "--monitor", strconv.FormatInt(int64(currentHandle.Pid), 10), "--monitor-identity-time", identityTime)
	monitorCtx, monitorCancel, monitorErr := MonitorPidFromFlags(context.Background(), logr.Discard())
	require.NoError(t, monitorErr)
	require.NoError(t, monitorCtx.Err(), "a running monitored process must not cancel the monitor context")
	monitorCancel()
	require.ErrorIs(t, context.Cause(monitorCtx), context.Canceled)

	for _, unusablePid := range []string{"0", "-1"} {
		parseMonitorFlags(t, "--monitor", unusablePid, "--monitor-identity-time", identityTime)
		unusableCtx, unusableCancel, unusableErr := MonitorPidFromFlags(context.Background(), logr.Discard())
		require.ErrorIs(t, unusableErr, process.ErrInvalidProcessHandle, "--monitor=%s", unusablePid)
		require.Error(t, unusableCtx.Err(), "--monitor=%s", unusablePid)
		unusableCancel()
	}

	parseMonitorFlags(t, "--monitor", "-1")
	unmonitoredCtx, unmonitoredCancel := GetMonitorContextFromFlags(context.Background(), logr.Discard())
	require.NoError(t, unmonitoredCtx.Err())
	unmonitoredCancel()
}

func parseMonitorFlags(t *testing.T, args ...string) {
	t.Helper()
	command := &cobra.Command{Use: "monitor-flags-test"}
	AddMonitorFlags(command)
	require.NoError(t, command.ParseFlags(args))
}

var _ logr.LogSink = (*blockingMonitorLogSink)(nil)
