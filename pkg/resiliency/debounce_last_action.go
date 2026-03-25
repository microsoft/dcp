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

type runStateAction struct {
	timer     *time.Timer
	threshold time.Time
}

// A variant of DebounceLast that calls an "action" (function with no return value).
// Because there is no return value, the Run method does not wait for the action to complete
// (the action is executed in a separate goroutine, fully asynchronously).
// The action will be called after the specified delay, but only if no new calls arrive in the meantime.
// If new calls arrive, the action will be delayed further, but no more than maxDelay.
type DebounceLastAction struct {
	delay    time.Duration
	maxDelay time.Duration
	run      *runStateAction
	m        *sync.Mutex
	action   func()
}

func NewDebounceLastAction(action func(), delay, maxDelay time.Duration) *DebounceLastAction {
	if maxDelay < delay {
		maxDelay = delay
	}

	return &DebounceLastAction{
		delay:    delay,
		maxDelay: maxDelay,
		action:   action,
		m:        &sync.Mutex{},
	}
}

func (dl *DebounceLastAction) Run(ctx context.Context) {
	dl.m.Lock()

	if dl.run == nil {
		// New run
		run := &runStateAction{
			timer:     time.NewTimer(dl.delay),
			threshold: time.Now().Add(dl.maxDelay),
		}
		dl.run = run
		dl.m.Unlock()

		go dl.execRunnerIfThresholdExceeded(ctx, run)
		return
	} else {
		if time.Now().Add(dl.delay).Before(dl.run.threshold) {
			dl.run.timer.Reset(dl.delay)
		}
	}

	dl.m.Unlock()
}

func (dl *DebounceLastAction) execRunnerIfThresholdExceeded(ctx context.Context, run *runStateAction) {
	select {
	case <-run.timer.C:
		dl.stopCurrentRun(run)
		dl.action()
	case <-ctx.Done():
		dl.stopCurrentRun(run)
	}
}

func (dl *DebounceLastAction) stopCurrentRun(run *runStateAction) {
	dl.m.Lock()
	defer dl.m.Unlock()
	run.timer.Stop()
	if dl.run == run {
		dl.run = nil
	}
}
