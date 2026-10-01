/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commands

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/internal/dcpproc/protocol"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/stretchr/testify/require"
)

type exitCodeSource interface {
	ExitCode() int
}

type processStopLogCounts struct {
	lock       sync.Mutex
	errorCalls int
	infoCalls  int
}

type processStopLogSink struct {
	counts *processStopLogCounts
}

func (*processStopLogSink) Init(logr.RuntimeInfo) {}

func (*processStopLogSink) Enabled(int) bool {
	return true
}

func (sink *processStopLogSink) Info(int, string, ...any) {
	sink.counts.lock.Lock()
	defer sink.counts.lock.Unlock()
	sink.counts.infoCalls++
}

func (sink *processStopLogSink) Error(error, string, ...any) {
	sink.counts.lock.Lock()
	defer sink.counts.lock.Unlock()
	sink.counts.errorCalls++
}

func (sink *processStopLogSink) WithValues(...any) logr.LogSink {
	return sink
}

func (sink *processStopLogSink) WithName(string) logr.LogSink {
	return sink
}

func TestStopProcessTreeCommandErrorUsesStructuredExitCodes(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name     string
		err      error
		exitCode int
	}{
		{
			name:     "incomplete tree",
			err:      process.ErrIncompleteProcessTree,
			exitCode: protocol.StopProcessTreeIncompleteExitCode,
		},
		{
			name:     "process gone",
			err:      &process.ErrProcessNotFound{Pid: 42},
			exitCode: protocol.StopProcessTreeProcessGoneExitCode,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			commandErr := stopProcessTreeCommandError(testCase.err)
			var exitErr exitCodeSource
			require.True(t, errors.As(commandErr, &exitErr))
			require.Equal(t, testCase.exitCode, exitErr.ExitCode())
			require.ErrorIs(t, commandErr, testCase.err)
		})
	}
}

// Verifies that helper cleanup starts from a fresh bounded context after the command context is canceled.
// The detached context must retain command-scoped values so cleanup logging and tracing still work.
func TestRunDetachedProcessCleanupIgnoresParentCancellation(t *testing.T) {
	t.Parallel()

	type contextKey struct{}
	parent, parentCancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "retained"))
	parentCancel()

	var cleanupErr error
	var cleanupValue any
	var cleanupDeadline time.Time
	var hasDeadline bool
	runErr := runDetachedProcessCleanup(parent, func(ctx context.Context) error {
		cleanupErr = ctx.Err()
		cleanupValue = ctx.Value(contextKey{})
		cleanupDeadline, hasDeadline = ctx.Deadline()
		return nil
	})

	require.NoError(t, runErr)
	require.NoError(t, cleanupErr)
	require.Equal(t, "retained", cleanupValue)
	require.True(t, hasDeadline)
	require.WithinDuration(t, time.Now().Add(21*time.Second), cleanupDeadline, time.Second)
}

// Verifies that an already-gone target is informational while incomplete cleanup and access failures remain errors.
func TestLogProcessStopFailureClassifiesGoneOutcome(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name               string
		err                error
		expectedErrorCalls int
		expectedInfoCalls  int
	}{
		{
			name:              "process gone",
			err:               &process.ErrProcessNotFound{Pid: 42},
			expectedInfoCalls: 1,
		},
		{
			name:               "incomplete process tree",
			err:                process.ErrIncompleteProcessTree,
			expectedErrorCalls: 1,
		},
		{
			name:               "generic failure",
			err:                errors.New("access denied"),
			expectedErrorCalls: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			counts := &processStopLogCounts{}
			logProcessStopFailure(
				logr.New(&processStopLogSink{counts: counts}),
				testCase.err,
				"already gone",
				"stop failed",
			)

			counts.lock.Lock()
			defer counts.lock.Unlock()
			require.Equal(t, testCase.expectedErrorCalls, counts.errorCalls)
			require.Equal(t, testCase.expectedInfoCalls, counts.infoCalls)
		})
	}
}
