// Copyright (c) Microsoft Corporation. All rights reserved.

package resiliency_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

func TestWorkQueueLimitsConcurrency(t *testing.T) {
	t.Parallel()

	const desiredMaxConcurrency = 3
	const workCount = 20
	m := &sync.Mutex{}
	concurrency := 0
	maxConcurrency := 0

	testCtx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()
	wq := resiliency.NewWorkQueue(testCtx, desiredMaxConcurrency)
	wg := &sync.WaitGroup{}
	wg.Add(workCount)

	for i := 0; i < workCount; i++ {
		err := wq.Enqueue(func(_ context.Context) {
			m.Lock()
			concurrency++
			m.Unlock()

			time.Sleep(time.Millisecond * 50)

			m.Lock()
			if concurrency > maxConcurrency {
				maxConcurrency = concurrency
			}
			concurrency--
			wg.Done()
			m.Unlock()
		})
		require.NoError(t, err)
	}

	wg.Wait()
	require.Equal(t, 0, concurrency)
	require.Equal(t, desiredMaxConcurrency, maxConcurrency)
}

func TestWorkQueueStopsProcessingAfterContextCancelled(t *testing.T) {
	t.Parallel()

	lifetimeCtx, cancelLifetimeCtx := testutil.GetTestContext(t, 10*time.Second)
	defer cancelLifetimeCtx()

	wq := resiliency.NewWorkQueue(lifetimeCtx, 1)
	var workDone uint32 = 0
	const workCount = 20

	for i := 0; i < workCount; i++ {
		err := wq.Enqueue(func(_ context.Context) {
			time.Sleep(time.Millisecond * 50)
			atomic.AddUint32(&workDone, 1)
		})
		require.NoError(t, err)
	}

	// Wait for a few work items to be processed
	time.Sleep(time.Millisecond * 300)

	cancelLifetimeCtx()
	workDoneRightAfterCancel := atomic.LoadUint32(&workDone)

	// Wait some more to make sure no more work is being done
	time.Sleep(time.Millisecond * 300)
	workDoneAfterWait := atomic.LoadUint32(&workDone)

	// We should have processed some, but not all, work items
	require.Greater(t, workDone, uint32(0))
	require.Less(t, workDone, uint32(workCount))

	require.LessOrEqual(t, workDoneAfterWait, workDoneRightAfterCancel+1, "we should have done at most one extra work item after the context was cancelled")

	err := wq.Enqueue(func(_ context.Context) {})
	require.Error(t, err, "should not be able to enqueue work after the context is cancelled")
}
