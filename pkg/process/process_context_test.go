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
