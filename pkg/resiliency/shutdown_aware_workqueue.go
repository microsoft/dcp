/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/microsoft/dcp/pkg/concurrency"
)

// ShutdownAwareWorkQueue preserves accepted work during a bounded shutdown drain.
// Work functions must respect their context; cancellation cannot forcibly stop running callbacks.
type ShutdownAwareWorkQueue struct {
	// Executes accepted work with bounded concurrency.
	innerQueue *WorkQueue

	// Stops admission and initiates shutdown when canceled.
	lifetimeCtx context.Context
	// Context passed to innerQueue; canceled after draining completes or times out.
	innerCtx context.Context
	// Cancels inner work after draining completes or times out.
	cancelInnerQueue context.CancelFunc

	// Maximum grace period for queued and running work.
	drainTimeout time.Duration

	// Buffers the single shutdown result for its consumer.
	completionErr chan error

	// Protects admission and pending-work accounting.
	lock sync.Mutex

	// Number of accepted work items that have not completed yet.
	pending uint

	// Frozen when shutdown begins, permanently stopping admission and notifying waiters.
	shutdownStarted *concurrency.AutoResetEvent

	// Closed when shutdown has begun and all accepted work items have completed.
	drained chan struct{}
}

// NewShutdownAwareWorkQueue wraps WorkQueue with lifetime-triggered graceful shutdown.
// A non-positive shutdownDrainTimeout gives unfinished work no grace period.
func NewShutdownAwareWorkQueue(
	lifetimeCtx context.Context,
	maxConcurrency uint8,
	shutdownDrainTimeout time.Duration,
) *ShutdownAwareWorkQueue {
	innerCtx, cancelInnerQueue := context.WithCancel(context.WithoutCancel(lifetimeCtx))
	wq := &ShutdownAwareWorkQueue{
		innerQueue:       NewWorkQueue(innerCtx, maxConcurrency),
		lifetimeCtx:      lifetimeCtx,
		innerCtx:         innerCtx,
		cancelInnerQueue: cancelInnerQueue,
		drainTimeout:     shutdownDrainTimeout,
		completionErr:    make(chan error, 1),
		shutdownStarted:  concurrency.NewAutoResetEvent(false),
		drained:          make(chan struct{}),
	}
	go func() {
		select {
		case <-lifetimeCtx.Done():
			wq.Shutdown()
		case <-wq.shutdownStarted.Wait():
		}
	}()
	return wq
}

// CompletionErr returns the buffered, single-consumer shutdown result channel.
// It receives nil on successful draining or a timeout error otherwise, then closes.
func (wq *ShutdownAwareWorkQueue) CompletionErr() <-chan error {
	return wq.completionErr
}

// Enqueue submits work while admission is open and accounts for it until its callback returns.
// Returns an error if work is nil, the lifetime context's error after cancellation, or context.Canceled after explicit shutdown.
func (wq *ShutdownAwareWorkQueue) Enqueue(work WorkQueueItem) error {
	if work == nil {
		return errors.New("work item cannot be nil")
	}
	wq.lock.Lock()
	defer wq.lock.Unlock()
	if lifetimeErr := wq.lifetimeCtx.Err(); lifetimeErr != nil {
		return lifetimeErr
	}
	if wq.shutdownStarted.Frozen() {
		return context.Canceled
	}

	wq.pending++
	enqueueErr := wq.innerQueue.Enqueue(func(ctx context.Context) {
		defer wq.completeWork()
		work(ctx)
	})
	if enqueueErr != nil {
		wq.pending--
	}
	return enqueueErr
}

// EnqueueAfter waits delay before scheduling work while accounting for it immediately.
// If shutdown begins during the wait, the wait ends early so accepted work can run within the drain window,
// or be discarded if the drain timeout has already elapsed.
func (wq *ShutdownAwareWorkQueue) EnqueueAfter(delay time.Duration, work WorkQueueItem) error {
	if work == nil {
		return errors.New("work item cannot be nil")
	}
	if delay <= 0 {
		return wq.Enqueue(work)
	}

	wq.lock.Lock()
	if lifetimeErr := wq.lifetimeCtx.Err(); lifetimeErr != nil {
		wq.lock.Unlock()
		return lifetimeErr
	}
	if wq.shutdownStarted.Frozen() {
		wq.lock.Unlock()
		return context.Canceled
	}
	wq.pending++
	wq.lock.Unlock()

	delayTimer := time.NewTimer(delay)
	go func() {
		defer delayTimer.Stop()
		select {
		case <-delayTimer.C:
		case <-wq.shutdownStarted.Wait():
		case <-wq.innerCtx.Done():
			wq.completeWork()
			return
		}

		enqueueErr := wq.innerQueue.Enqueue(func(ctx context.Context) {
			defer wq.completeWork()
			work(ctx)
		})
		if enqueueErr != nil {
			wq.completeWork()
		}
	}()
	return nil
}

// Shutdown stops accepting work and initiates bounded draining without blocking.
// It is safe to call concurrently or more than once.
func (wq *ShutdownAwareWorkQueue) Shutdown() {
	wq.lock.Lock()
	defer wq.lock.Unlock()
	if wq.shutdownStarted.Frozen() {
		return
	}
	wq.shutdownStarted.SetAndFreeze()
	if wq.pending == 0 {
		close(wq.drained)
	}
	drainTimer := time.NewTimer(wq.drainTimeout)
	go wq.finishShutdown(drainTimer)
}

func (wq *ShutdownAwareWorkQueue) completeWork() {
	wq.lock.Lock()
	defer wq.lock.Unlock()
	wq.pending--
	if wq.shutdownStarted.Frozen() && wq.pending == 0 {
		close(wq.drained)
	}
}

func (wq *ShutdownAwareWorkQueue) finishShutdown(drainTimer *time.Timer) {
	defer drainTimer.Stop()
	var completionErr error
	select {
	case <-wq.drained:
	default:
		select {
		case <-wq.drained:
		case <-drainTimer.C:
			completionErr = fmt.Errorf(
				"work queue shutdown drain timeout of %s exceeded; unfinished work was canceled or discarded: %w",
				wq.drainTimeout,
				context.DeadlineExceeded,
			)
		}
	}
	wq.cancelInnerQueue()
	wq.completionErr <- completionErr
	close(wq.completionErr)
}
