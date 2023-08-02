package resiliency

import (
	"context"
	"sync"
	"time"
)

type Runner[T any, R any] interface {
	~func(T) (R, error)
}

type ResultWithError[R any] struct {
	V   R
	Err error
}

type DebounceLast[T any, R any, RF Runner[T, R]] struct {
	delay     time.Duration
	threshold time.Time
	runC      chan struct{}
	m         *sync.Mutex
	runner    RF
	res       *ResultWithError[R]
}

func NewDebounceLast[T any, R any, RF Runner[T, R]](runner RF, delay time.Duration) DebounceLast[T, R, RF] {
	return DebounceLast[T, R, RF]{
		delay:  delay,
		runner: runner,
		m:      &sync.Mutex{},
	}
}

func (dl *DebounceLast[T, R, RF]) Run(ctx context.Context, arg T) (R, error) {
	dl.m.Lock()

	// Extend the threshold
	dl.threshold = time.Now().Add(dl.delay)
	var runC chan struct{}
	var res *ResultWithError[R]

	if dl.runC == nil {
		// New run
		dl.runC = make(chan struct{}, 1)
		dl.res = &ResultWithError[R]{}
		runC = dl.runC
		res = dl.res

		// How often we will be checking that no new calls have arrived and it is time to run the runner.
		// Could be made a parameter to the constructor func.
		ticker := time.NewTicker(time.Millisecond * 50)

		go dl.execRunnerIfThresholdExceeded(ctx, arg, ticker)
	} else {
		// Run in progress
		runC = dl.runC
		res = dl.res
	}
	dl.m.Unlock()
	<-runC
	return res.V, res.Err
}

// The helper goroutine that will be woken up periodically and run the runner if the threshold is exceeded.
func (dl *DebounceLast[T, R, RF]) execRunnerIfThresholdExceeded(ctx context.Context, arg T, ticker *time.Ticker) {
	// No matter how we exit this, we want to reset the run
	defer func() {
		ticker.Stop()
		close(dl.runC)
		dl.runC = nil
		dl.m.Unlock()
	}()

	for {
		select {
		case <-ticker.C:
			dl.m.Lock()
			if time.Now().After(dl.threshold) {
				dl.res.V, dl.res.Err = dl.runner(arg)
				return
			}
			dl.m.Unlock()
		case <-ctx.Done():
			dl.m.Lock()
			dl.res.V, dl.res.Err = *new(R), ctx.Err()
			return
		}
	}
}
