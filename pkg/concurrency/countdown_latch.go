/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

import (
	"context"
	"sync"
)

// CountdownLatch tracks dynamically registered tasks until closing begins.
// Unlike a fixed-count Java latch, tasks may register until Close starts.
// The zero value is ready to use and must not be copied after first use.
type CountdownLatch struct {
	lock           sync.Mutex
	closing        bool
	closingStarted chan struct{}
	tasks          sync.WaitGroup
}

// NewCountdownLatch creates a CountdownLatch ready to accept registrations.
func NewCountdownLatch() *CountdownLatch {
	return &CountdownLatch{}
}

// Start waits for cancellation, then closes the latch and waits for accepted completions.
// It returns nil after shutdown work finishes.
func (latch *CountdownLatch) Start(ctx context.Context) error {
	<-ctx.Done()
	latch.Close()
	latch.Wait()
	return nil
}

// Register reserves a task slot unless closing has begun and returns its completion callback.
// The completion callback must be called exactly once for every accepted registration.
func (latch *CountdownLatch) Register() (func(), bool) {
	latch.lock.Lock()
	defer latch.lock.Unlock()
	if latch.closing {
		return nil, false
	}

	latch.tasks.Add(1)
	return latch.tasks.Done, true
}

// Close atomically rejects future registrations and releases callers waiting for closing to begin.
// It does not wait for accepted completions and is safe to call concurrently or more than once.
func (latch *CountdownLatch) Close() {
	latch.lock.Lock()
	defer latch.lock.Unlock()
	if latch.closing {
		return
	}

	closingStarted := latch.closingStartedLocked()
	latch.closing = true
	close(closingStarted)
}

// Wait waits for closing to begin and then for every accepted registration to report completion.
// It may be called concurrently by multiple callers before or after Close.
func (latch *CountdownLatch) Wait() {
	latch.lock.Lock()
	closingStarted := latch.closingStartedLocked()
	latch.lock.Unlock()

	<-closingStarted
	latch.tasks.Wait()
}

func (latch *CountdownLatch) closingStartedLocked() chan struct{} {
	if latch.closingStarted == nil {
		latch.closingStarted = make(chan struct{})
	}
	return latch.closingStarted
}
