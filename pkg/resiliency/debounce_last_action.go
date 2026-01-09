/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

import (
	"context"
	"sync"
	"time"
)

// A variant of DebounceLast that calls an "action" (function with no return value).
// Because there is no return value, the Run method does not wait for the action to complete
// (the action is executed in a separate goroutine, fully asynchronously).
// The action will be called after the specified delay, but only if no new calls arrive in the meantime.
// If new calls arrive, the action will be delayed further, but no more than maxDelay.
type DebounceLastAction[T any] struct {
	delay     time.Duration
	maxDelay  time.Duration
	timer     *time.Timer
	threshold time.Time
	runC      chan struct{}
	m         *sync.Mutex
	action    func(T)
}

func NewDebounceLastAction[T any](action func(T), delay, maxDelay time.Duration) *DebounceLastAction[T] {
	if maxDelay < delay {
		maxDelay = delay
	}

	return &DebounceLastAction[T]{
		delay:    delay,
		maxDelay: maxDelay,
		action:   action,
		m:        &sync.Mutex{},
	}
}

func (dl *DebounceLastAction[T]) Run(ctx context.Context, arg T) {
	dl.m.Lock()
	defer dl.m.Unlock()

	if dl.runC == nil {
		// New run
		dl.timer = time.NewTimer(dl.delay)
		dl.runC = make(chan struct{}, 1)
		dl.threshold = time.Now().Add(dl.maxDelay)
		go dl.execRunnerIfThresholdExceeded(ctx, arg)
	} else {
		if time.Now().Add(dl.delay).Before(dl.threshold) {
			dl.timer.Reset(dl.delay)
		}
	}
}

func (dl *DebounceLastAction[T]) execRunnerIfThresholdExceeded(ctx context.Context, arg T) {
	select {
	case <-dl.timer.C:
		dl.stopCurrentRun()
		dl.action(arg)
	case <-ctx.Done():
		dl.stopCurrentRun()
	}
}

func (dl *DebounceLastAction[T]) stopCurrentRun() {
	dl.m.Lock()
	defer dl.m.Unlock()
	dl.timer.Stop()
	close(dl.runC)
	dl.runC = nil
	dl.threshold = time.Time{}
}
