/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package testutil

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/require"
)

// Verifies that the test executor returns the same process-gone and
// identity-mismatch classifications as the production executor.
func TestTestProcessExecutorMatchesGoneAndIdentityErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	executor := NewTestProcessExecutor(ctx)

	missingErr := executor.StopProcess(ctx, process.NewHandle(99, time.Unix(1, 0).UTC()))
	require.True(t, process.IsProcessGoneErr(missingErr))

	handle, _, startErr := executor.StartProcess(ctx, exec.Command("test"), nil, process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	mismatchErr := executor.StopProcess(ctx, process.NewHandle(handle.Pid, handle.IdentityTime.Add(time.Minute)))
	require.ErrorIs(t, mismatchErr, process.ErrProcessIdentityMismatch)
	require.True(t, process.IsProcessGoneErr(mismatchErr))
}

// Verifies that stopping an already-finished test execution is idempotent and
// does not deliver a duplicate exit callback.
func TestTestProcessExecutorDoesNotCompleteFinishedExecutionTwice(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()
	executor := NewTestProcessExecutor(ctx)
	var completionCount atomic.Int32
	handle, startWaiting, startErr := executor.StartProcess(
		ctx,
		exec.Command("test"),
		process.ProcessExitHandlerFunc(func(process.Pid_t, int32, error) {
			completionCount.Add(1)
		}),
		process.CreationFlagsNone,
		nil,
	)
	require.NoError(t, startErr)
	startWaiting()

	require.NoError(t, executor.StopProcess(ctx, handle))
	require.Eventually(t, func() bool {
		return completionCount.Load() == 1
	}, time.Second, time.Millisecond)

	require.NoError(t, executor.StopProcess(ctx, handle))
	require.Never(t, func() bool {
		return completionCount.Load() > 1
	}, 100*time.Millisecond, time.Millisecond)
}

// Verifies that the test executor observes caller cancellation before
// attempting process lookup or mutation.
func TestTestProcessExecutorStopRespectsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor := NewTestProcessExecutor(context.Background())

	stopErr := executor.StopProcess(ctx, process.NewHandle(1, time.Unix(1, 0).UTC()))
	require.ErrorIs(t, stopErr, context.Canceled)
	require.False(t, errors.Is(stopErr, process.ErrProcessIdentityMismatch))
}
