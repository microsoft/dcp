/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that ShutdownAwareWorkQueue rejects new submissions during shutdown while
// preserving queued and running work until completion, for both supported shutdown triggers.
func TestShutdownAwareWorkQueueDrainsAcceptedWork(t *testing.T) {
	t.Parallel()
	for _, cancelLifetime := range []bool{false, true} {
		name := "explicit-shutdown"
		if cancelLifetime {
			name = "lifetime-cancellation"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
			defer cancel()
			lifetimeCtx, stopLifetime := context.WithCancel(ctx)
			defer stopLifetime()
			queue := resiliency.NewShutdownAwareWorkQueue(lifetimeCtx, 1, time.Minute)
			require.Error(t, queue.Enqueue(nil))
			require.Error(t, queue.EnqueueAfter(time.Second, nil))
			started := make(chan struct{})
			release := make(chan struct{})
			finish := sync.OnceFunc(func() { close(release) })
			defer finish()
			var completed atomic.Uint32
			var runningCtx context.Context
			require.NoError(t, queue.Enqueue(func(workCtx context.Context) {
				runningCtx = workCtx
				close(started)
				select {
				case <-release:
				case <-workCtx.Done():
				}
				completed.Add(1)
			}))
			require.NoError(t, queue.Enqueue(func(workCtx context.Context) {
				if workCtx.Err() != nil {
					t.Errorf("Queued work lost its shutdown grace period: %v", workCtx.Err())
				}
				completed.Add(1)
			}))
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("The first work item did not start")
			}

			if cancelLifetime {
				stopLifetime()
			} else {
				queue.Shutdown()
				queue.Shutdown()
				require.NoError(t, lifetimeCtx.Err(), "Explicit shutdown must not cancel the caller's context")
			}
			require.ErrorIs(t, queue.Enqueue(func(context.Context) {}), context.Canceled)
			require.NoError(t, runningCtx.Err(), "Accepted work must retain its context while draining")
			select {
			case <-queue.CompletionErr():
				t.Fatal("Shutdown completed while accepted work was still running")
			default:
			}
			finish()
			require.NoError(t, awaitShutdownAwareQueueCompletion(t, ctx, queue))
			require.Equal(t, uint32(2), completed.Load())
			require.ErrorIs(t, runningCtx.Err(), context.Canceled, "Successful shutdown must release the inner context")
		})
	}
}

// Verifies that ShutdownAwareWorkQueue bounds draining, cancels running work, and
// discards queued work without waiting for a running callback to return after cancellation.
func TestShutdownAwareWorkQueueCancelsUnfinishedWork(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "positive", timeout: 50 * time.Millisecond},
		{name: "zero"},
		{name: "negative", timeout: -time.Second},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
			defer cancel()
			queue := resiliency.NewShutdownAwareWorkQueue(ctx, 1, testCase.timeout)
			started := make(chan struct{})
			returned := make(chan struct{})
			release := make(chan struct{})
			finish := sync.OnceFunc(func() { close(release) })
			defer finish()
			var runningCtx context.Context
			var queuedWorkRan atomic.Bool
			require.NoError(t, queue.Enqueue(func(workCtx context.Context) {
				defer close(returned)
				runningCtx = workCtx
				close(started)
				<-workCtx.Done()
				<-release
			}))
			require.NoError(t, queue.Enqueue(func(context.Context) { queuedWorkRan.Store(true) }))
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("The first work item did not start")
			}

			shutdownStarted := time.Now()
			queue.Shutdown()
			completionErr := awaitShutdownAwareQueueCompletion(t, ctx, queue)
			require.ErrorIs(t, completionErr, context.DeadlineExceeded)
			require.ErrorContains(t, completionErr, "unfinished work")
			if testCase.timeout > 0 {
				require.GreaterOrEqual(t, time.Since(shutdownStarted), testCase.timeout)
			}
			require.ErrorIs(t, runningCtx.Err(), context.Canceled)
			require.False(t, queuedWorkRan.Load())
			finish()
			select {
			case <-returned:
			case <-ctx.Done():
				t.Fatal("The running callback did not return after release")
			}
			require.False(t, queuedWorkRan.Load(), "Discarded work must not start when the previous worker exits")
		})
	}
}

// Verifies that ShutdownAwareWorkQueue accounts for every submission accepted
// concurrently with idempotent shutdown calls and rejects later submissions.
func TestShutdownAwareWorkQueueSerializesAdmissionWithShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()
	queue := resiliency.NewShutdownAwareWorkQueue(ctx, 2, time.Minute)
	release := make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	require.NoError(t, queue.Enqueue(func(workCtx context.Context) {
		select {
		case <-release:
		case <-workCtx.Done():
		}
	}))
	var accepted, completed atomic.Uint32
	var submissions sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		submissions.Go(func() {
			<-start
			enqueueErr := queue.Enqueue(func(context.Context) { completed.Add(1) })
			if enqueueErr == nil {
				accepted.Add(1)
			} else if !errors.Is(enqueueErr, context.Canceled) {
				t.Errorf("Unexpected enqueue error: %v", enqueueErr)
			}
		})
	}
	for range 8 {
		submissions.Go(func() {
			<-start
			queue.Shutdown()
		})
	}
	close(start)
	submissions.Wait()
	finish()
	require.NoError(t, awaitShutdownAwareQueueCompletion(t, ctx, queue))
	require.Equal(t, accepted.Load(), completed.Load())
	require.ErrorIs(t, queue.Enqueue(func(context.Context) {}), context.Canceled)
}

// Verifies that ShutdownAwareWorkQueue successfully completes an empty drain
// without a grace period, including construction with an already-canceled lifetime.
func TestShutdownAwareWorkQueueCompletesEmptyDrain(t *testing.T) {
	t.Parallel()
	for _, alreadyCanceled := range []bool{false, true} {
		name := "explicit-shutdown"
		if alreadyCanceled {
			name = "already-canceled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
			defer cancel()
			lifetimeCtx, stopLifetime := context.WithCancel(ctx)
			defer stopLifetime()
			if alreadyCanceled {
				stopLifetime()
			}
			queue := resiliency.NewShutdownAwareWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency, 0)
			if !alreadyCanceled {
				queue.Shutdown()
			}
			require.NoError(t, awaitShutdownAwareQueueCompletion(t, ctx, queue))
			require.ErrorIs(t, queue.Enqueue(func(context.Context) {}), context.Canceled)
		})
	}
}

func awaitShutdownAwareQueueCompletion(t *testing.T, ctx context.Context, queue *resiliency.ShutdownAwareWorkQueue) error {
	t.Helper()
	select {
	case completionErr, received := <-queue.CompletionErr():
		require.True(t, received, "The completion channel closed without a result")
		return completionErr
	case <-ctx.Done():
		t.Fatal("Shutdown did not report completion before the test context ended")
		return ctx.Err()
	}
}

// Verifies that ShutdownAwareWorkQueue.EnqueueAfter does not occupy worker slots while waiting,
// wakes delayed items immediately when shutdown begins so they execute within the drain window,
// and cancels woken items if the drain timeout expires.
func TestShutdownAwareWorkQueueWakesDelayedWorkOnShutdown(t *testing.T) {
	t.Parallel()

	t.Run("drains-woken-work", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
		defer cancel()

		queue := resiliency.NewShutdownAwareWorkQueue(ctx, 1, time.Minute)
		var delayedRan atomic.Bool
		require.NoError(t, queue.EnqueueAfter(time.Hour, func(workCtx context.Context) {
			if workCtx.Err() != nil {
				t.Errorf("Woken delayed work lost its drain context: %v", workCtx.Err())
			}
			delayedRan.Store(true)
		}))

		// Verify the single concurrency slot is not occupied while EnqueueAfter waits.
		immediateDone := make(chan struct{})
		require.NoError(t, queue.Enqueue(func(context.Context) {
			close(immediateDone)
		}))
		select {
		case <-immediateDone:
		case <-ctx.Done():
			t.Fatal("Delayed work item blocked immediate execution on a 1-worker queue")
		}
		require.False(t, delayedRan.Load())

		queue.Shutdown()
		require.ErrorIs(t, queue.EnqueueAfter(time.Second, func(context.Context) {}), context.Canceled)
		require.NoError(t, awaitShutdownAwareQueueCompletion(t, ctx, queue))
		require.True(t, delayedRan.Load(), "Delayed work item must wake and complete during shutdown drain")
	})

	t.Run("cancels-woken-work-on-timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
		defer cancel()

		queue := resiliency.NewShutdownAwareWorkQueue(ctx, 1, 50*time.Millisecond)
		wokenStarted := make(chan struct{})
		wokenCanceled := make(chan struct{})
		require.NoError(t, queue.EnqueueAfter(time.Hour, func(workCtx context.Context) {
			close(wokenStarted)
			<-workCtx.Done()
			close(wokenCanceled)
		}))

		queue.Shutdown()
		select {
		case <-wokenStarted:
		case <-ctx.Done():
			t.Fatal("Delayed work item did not wake on shutdown")
		}
		completionErr := awaitShutdownAwareQueueCompletion(t, ctx, queue)
		require.ErrorIs(t, completionErr, context.DeadlineExceeded)
		select {
		case <-wokenCanceled:
		case <-ctx.Done():
			t.Fatal("Drain timeout did not cancel woken delayed work")
		}
	})
}

// Verifies that ShutdownAwareWorkQueue.EnqueueAfter runs delayed work on a live queue no earlier than
// its delay, admits work with a non-positive delay immediately in submission order, and drains cleanly.
func TestShutdownAwareWorkQueueRunsDelayedWorkOnLiveQueue(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	queue := resiliency.NewShutdownAwareWorkQueue(ctx, 1, time.Minute)

	const delay = 50 * time.Millisecond
	delayedRanAt := make(chan time.Time, 1)
	enqueuedAt := time.Now()
	require.NoError(t, queue.EnqueueAfter(delay, func(workCtx context.Context) {
		if workCtx.Err() != nil {
			t.Errorf("Delayed work ran with a canceled context: %v", workCtx.Err())
		}
		delayedRanAt <- time.Now()
	}))
	select {
	case ranAt := <-delayedRanAt:
		require.GreaterOrEqual(t, ranAt.Sub(enqueuedAt), delay, "Delayed work must not run before its delay elapses")
	case <-ctx.Done():
		t.Fatal("Delayed work did not run on a live queue")
	}

	// Occupy the single worker so the order in which later work is admitted becomes observable.
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseBlocker) })
	defer release()
	require.NoError(t, queue.Enqueue(func(workCtx context.Context) {
		close(blockerStarted)
		select {
		case <-releaseBlocker:
		case <-workCtx.Done():
		}
	}))
	select {
	case <-blockerStarted:
	case <-ctx.Done():
		t.Fatal("Blocking work item did not start")
	}

	var completionOrder []string
	var orderLock sync.Mutex
	recordCompletion := func(name string) resiliency.WorkQueueItem {
		return func(context.Context) {
			orderLock.Lock()
			completionOrder = append(completionOrder, name)
			orderLock.Unlock()
		}
	}
	require.NoError(t, queue.EnqueueAfter(0, recordCompletion("zero-delay")))
	require.NoError(t, queue.EnqueueAfter(-time.Second, recordCompletion("negative-delay")))
	require.NoError(t, queue.Enqueue(recordCompletion("enqueued")))
	release()

	queue.Shutdown()
	require.NoError(t, awaitShutdownAwareQueueCompletion(t, ctx, queue))
	orderLock.Lock()
	defer orderLock.Unlock()
	require.Equal(t, []string{"zero-delay", "negative-delay", "enqueued"}, completionOrder)
}
