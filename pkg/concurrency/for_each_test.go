/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestForEachBoundedLimitsConcurrency(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()

	const maxConcurrency = 3
	items := []int{1, 2, 3, 4, 5, 6}
	started := make(chan struct{}, len(items))
	release := make(chan struct{})
	completed := make(chan error, 1)
	var active atomic.Int32
	var maximumActive atomic.Int32

	go func() {
		completed <- concurrency.ForEachBounded(ctx, items, maxConcurrency, func(ctx context.Context, _ int) error {
			currentActive := active.Add(1)
			defer active.Add(-1)
			for {
				observedMaximum := maximumActive.Load()
				if currentActive <= observedMaximum || maximumActive.CompareAndSwap(observedMaximum, currentActive) {
					break
				}
			}
			started <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		})
	}()

	for range maxConcurrency {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-started:
		}
	}
	select {
	case <-started:
		require.Fail(t, "action exceeded the configured concurrency limit")
	default:
	}

	close(release)
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case iterationErr := <-completed:
		require.NoError(t, iterationErr)
	}
	require.Equal(t, int32(maxConcurrency), maximumActive.Load())
}

func TestForEachBoundedStopsAfterError(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()

	expectedErr := errors.New("action failed")
	var calls atomic.Int32
	actionErr := concurrency.ForEachBounded(ctx, []int{1, 2, 3}, 1, func(context.Context, int) error {
		calls.Add(1)
		return expectedErr
	})

	require.ErrorIs(t, actionErr, expectedErr)
	require.Equal(t, int32(1), calls.Load())
}

func TestForEachBoundedRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	require.Error(t, concurrency.ForEachBounded(context.Background(), []int{1}, 0, func(context.Context, int) error {
		return nil
	}))
	require.Error(t, concurrency.ForEachBounded[int](context.Background(), []int{1}, 1, nil))
}
