/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type processContextTestKey struct{}

// Verifies that WithStopTimeout adds the process-stop deadline
// and still propagates cancellation from its parent context.
func TestWithStopTimeoutPreservesParentCancellation(t *testing.T) {
	t.Parallel()

	parent, parentCancel := context.WithCancel(context.Background())
	before := time.Now()
	ctx, cancel := WithStopTimeout(parent)
	defer cancel()

	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.WithinDuration(t, before.Add(processStopTimeout), deadline, time.Second)
	parentCancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// Verifies that WithDetachedStopTimeout ignores parent cancellation, retains context values,
// and creates a fresh bounded process-stop deadline.
func TestWithDetachedStopTimeoutUsesFreshDeadlineAndRetainsValues(t *testing.T) {
	t.Parallel()

	parent, parentCancel := context.WithCancel(context.WithValue(context.Background(), processContextTestKey{}, "value"))
	parentCancel()

	before := time.Now()
	ctx, cancel := WithDetachedStopTimeout(parent)
	defer cancel()

	require.NoError(t, ctx.Err())
	require.Equal(t, "value", ctx.Value(processContextTestKey{}))
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.WithinDuration(t, before.Add(processStopTimeout), deadline, time.Second)
}

// Verifies that the monitored process stop timeout includes the reporting margin
// while still propagating parent cancellation.
func TestWithMonitoredProcessStopTimeoutPreservesParentCancellation(t *testing.T) {
	t.Parallel()

	parent, parentCancel := context.WithCancel(context.Background())
	before := time.Now()
	ctx, cancel := WithMonitoredProcessStopTimeout(parent)
	defer cancel()

	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.WithinDuration(t, before.Add(monitoredProcessStopTimeout), deadline, time.Second)
	parentCancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// Verifies that detached monitored process cleanup ignores parent cancellation,
// retains context values, and receives a fresh bounded deadline.
func TestWithDetachedMonitoredProcessStopTimeoutUsesFreshDeadlineAndRetainsValues(t *testing.T) {
	t.Parallel()

	parent, parentCancel := context.WithCancel(context.WithValue(context.Background(), processContextTestKey{}, "value"))
	parentCancel()

	before := time.Now()
	ctx, cancel := WithDetachedMonitoredProcessStopTimeout(parent)
	defer cancel()

	require.NoError(t, ctx.Err())
	require.Equal(t, "value", ctx.Value(processContextTestKey{}))
	deadline, hasDeadline := ctx.Deadline()
	require.True(t, hasDeadline)
	require.WithinDuration(t, before.Add(monitoredProcessStopTimeout), deadline, time.Second)
}
