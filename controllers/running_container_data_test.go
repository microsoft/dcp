/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/internal/termpty"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/process"
)

type terminalCleanupContextKey struct{}

type terminalCleanupTestExecutor struct {
	process.Executor

	stopContextErr error
	stopValue      any
	hasDeadline    bool
	stopCalls      int
	stopErr        error
}

func (executor *terminalCleanupTestExecutor) StopProcess(
	ctx context.Context,
	_ process.ProcessHandle,
	_ ...process.ProcessStopOption,
) error {
	executor.stopCalls++
	executor.stopContextErr = ctx.Err()
	executor.stopValue = ctx.Value(terminalCleanupContextKey{})
	_, executor.hasDeadline = ctx.Deadline()
	return executor.stopErr
}

type terminalCleanupLogCounts struct {
	lock       sync.Mutex
	errorCalls int
	infoCalls  int
}

type terminalCleanupLogSink struct {
	counts *terminalCleanupLogCounts
}

func (*terminalCleanupLogSink) Init(logr.RuntimeInfo) {}

func (*terminalCleanupLogSink) Enabled(int) bool {
	return true
}

func (sink *terminalCleanupLogSink) Info(int, string, ...any) {
	sink.counts.lock.Lock()
	defer sink.counts.lock.Unlock()
	sink.counts.infoCalls++
}

func (sink *terminalCleanupLogSink) Error(error, string, ...any) {
	sink.counts.lock.Lock()
	defer sink.counts.lock.Unlock()
	sink.counts.errorCalls++
}

func (sink *terminalCleanupLogSink) WithValues(...any) logr.LogSink {
	return sink
}

func (sink *terminalCleanupLogSink) WithName(string) logr.LogSink {
	return sink
}

// Verifies that terminal cleanup detaches a canceled parent context, retains its values,
// applies a stop deadline, and clears the stored terminal resources.
func TestCloseTerminalResourcesDetachesStopFromCanceledContext(t *testing.T) {
	t.Parallel()

	parentCtx, parentCancel := context.WithCancel(context.WithValue(
		context.Background(),
		terminalCleanupContextKey{},
		"retained",
	))
	parentCancel()

	testPty := internal_testutil.NewTestPty()
	t.Cleanup(func() { _ = testPty.Close() })
	executor := &terminalCleanupTestExecutor{}
	rcd := &runningContainerData{
		ptp: &termpty.PseudoTerminalProcess{
			PTY:         testPty,
			Handle:      process.NewHandle(4300, time.Unix(1000, 0).UTC()),
			ExitHandler: process.NewConcurrentProcessExitHandler(),
			Executor:    executor,
		},
	}

	rcd.closeTerminalResources(parentCtx, executor, logr.Discard())

	require.Equal(t, 1, executor.stopCalls)
	require.NoError(t, executor.stopContextErr)
	require.Equal(t, "retained", executor.stopValue)
	require.True(t, executor.hasDeadline)
	require.Nil(t, rcd.ptp)
	require.Nil(t, rcd.connMgr)
}

// Verifies that terminal attach stop races classified as process-gone stay informational.
// Access and other genuine stop failures must remain error-level diagnostics.
func TestCloseTerminalResourcesClassifiesStopFailures(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name               string
		stopErr            error
		expectedErrorCalls int
		expectedInfoCalls  int
	}{
		{
			name:              "process gone",
			stopErr:           fmt.Errorf("identity check: %w", process.ErrProcessIdentityMismatch),
			expectedInfoCalls: 1,
		},
		{
			name:               "genuine failure",
			stopErr:            errors.New("access denied"),
			expectedErrorCalls: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			testPty := internal_testutil.NewTestPty()
			t.Cleanup(func() { _ = testPty.Close() })
			executor := &terminalCleanupTestExecutor{stopErr: testCase.stopErr}
			rcd := &runningContainerData{
				ptp: &termpty.PseudoTerminalProcess{
					PTY:         testPty,
					Handle:      process.NewHandle(4301, time.Unix(1001, 0).UTC()),
					ExitHandler: process.NewConcurrentProcessExitHandler(),
					Executor:    executor,
				},
			}
			counts := &terminalCleanupLogCounts{}
			log := logr.New(&terminalCleanupLogSink{counts: counts})

			rcd.closeTerminalResources(context.Background(), executor, log)

			counts.lock.Lock()
			defer counts.lock.Unlock()
			require.Equal(t, testCase.expectedErrorCalls, counts.errorCalls)
			require.Equal(t, testCase.expectedInfoCalls, counts.infoCalls)
		})
	}
}
