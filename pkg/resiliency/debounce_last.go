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

// DebounceLast calls the runner function after the specified delay, but only if no new calls have arrived in the meantime.
// If new calls arrive, the runner will be delayed further, but no more than maxDelay.
// After the runner function completes, the callers of Run() will all receive the same result (and error, if any).
type DebounceLast[R any] struct {
	delay     time.Duration
	maxDelay  time.Duration
	threshold time.Time
	timer     *time.Timer
	runC      chan struct{}
	m         *sync.Mutex
	runner    func() (R, error)
	res       *ResultWithError[R]
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

	var runC chan struct{}
	var res *ResultWithError[R]

	if dl.runC == nil {
		// New run
		dl.timer = time.NewTimer(dl.delay)
		dl.runC = make(chan struct{}, 1)
		dl.threshold = time.Now().Add(dl.maxDelay)
		dl.res = &ResultWithError[R]{}
		runC = dl.runC
		res = dl.res

		go dl.execRunnerIfThresholdExceeded(ctx)
	} else {
		// Run in progress
		runC = dl.runC
		res = dl.res
		if time.Now().Add(dl.delay).Before(dl.threshold) {
			dl.timer.Reset(dl.delay)
		}
	}
	dl.m.Unlock()

	<-runC
	return res.V, res.Err
}

// The helper goroutine that will be woken up periodically and run the runner if the threshold is exceeded.
func (dl *DebounceLast[R]) execRunnerIfThresholdExceeded(ctx context.Context) {
	defer func() {
		dl.timer.Stop()
		close(dl.runC)
		dl.runC = nil
		dl.threshold = time.Time{}
		dl.m.Unlock()
	}()

	select {

	case <-dl.timer.C:
		func() {
			var val R
			var err error
			func() {
				defer dl.m.Lock()
				val, err = dl.runner()
			}()
			dl.res.V = val
			dl.res.Err = err
		}()

	case <-ctx.Done():
		dl.m.Lock()
		dl.res.V, dl.res.Err = *new(R), ctx.Err()
	}
}
