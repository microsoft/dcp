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

type ResultWithError[R any] struct {
	V   R
	Err error
}

type runState[R any] struct {
	timer     *time.Timer
	threshold time.Time
	doneC     chan struct{}
	res       ResultWithError[R]
}

// DebounceLast calls the runner function after the specified delay, but only if no new calls have arrived in the meantime.
// If new calls arrive, the runner will be delayed further, but no more than maxDelay.
// After the runner function completes, the callers of Run() will all receive the same result (and error, if any).
type DebounceLast[R any] struct {
	delay    time.Duration
	maxDelay time.Duration
	run      *runState[R]
	m        *sync.Mutex
	runner   func() (R, error)
}

func NewDebounceLast[R any](runner func() (R, error), delay, maxDelay time.Duration) *DebounceLast[R] {
	return &DebounceLast[R]{
		delay:    delay,
		maxDelay: maxDelay,
		runner:   runner,
		m:        &sync.Mutex{},
	}
}

func (dl *DebounceLast[R]) Run(ctx context.Context) (R, error) {
	dl.m.Lock()

	var run *runState[R]

	if dl.run == nil {
		// New run
		run = &runState[R]{
			timer:     time.NewTimer(dl.delay),
			threshold: time.Now().Add(dl.maxDelay),
			doneC:     make(chan struct{}),
		}
		dl.run = run

		go dl.execRunnerIfThresholdExceeded(ctx, run)
	} else {
		// Run in progress
		run = dl.run
		if time.Now().Add(dl.delay).Before(run.threshold) {
			run.timer.Reset(dl.delay)
		}
	}
	dl.m.Unlock()

	<-run.doneC
	return run.res.V, run.res.Err
}

func (dl *DebounceLast[R]) execRunnerIfThresholdExceeded(ctx context.Context, run *runState[R]) {
	var val R
	var err error

	select {
	case <-run.timer.C:
		dl.stopCurrentRun(run)
		val, err = dl.runner()

	case <-ctx.Done():
		dl.stopCurrentRun(run)
		val, err = *new(R), ctx.Err()
	}

	run.timer.Stop()
	run.res.V = val
	run.res.Err = err
	close(run.doneC)
}

func (dl *DebounceLast[R]) stopCurrentRun(run *runState[R]) {
	dl.m.Lock()
	defer dl.m.Unlock()

	if dl.run == run {
		dl.run = nil
	}
}
