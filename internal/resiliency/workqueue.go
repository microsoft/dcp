package resiliency

import (
	"context"
	"math"
	"runtime"

	"github.com/smallnest/chanx"
)

const DefaultConcurrency uint8 = 0

type WorkQueueItem = func(ctx context.Context)

// WorkQueue runs work concurrently, but limits the number of concurrent executions.
type WorkQueue struct {
	incoming    *chanx.UnboundedChan[WorkQueueItem]
	limiter     chan struct{}
	lifetimeCtx context.Context
}

func NewWorkQueue(lifetimeCtx context.Context, maxConcurrency uint8) *WorkQueue {
	if maxConcurrency == DefaultConcurrency {
		maxConcurrency = getDefaultConcurrency()
	}

	wq := WorkQueue{
		// The maxConcurrency parameter used here indicates the initial size of the incoming work channel;
		// the channel itself is unbonunded.
		incoming: chanx.NewUnboundedChan[WorkQueueItem](lifetimeCtx, int(maxConcurrency)),

		limiter:     make(chan struct{}, maxConcurrency),
		lifetimeCtx: lifetimeCtx,
	}
	go wq.doWork()
	return &wq
}

func (wq *WorkQueue) Enqueue(work WorkQueueItem) error {
	if wq.lifetimeCtx.Err() != nil {
		return wq.lifetimeCtx.Err()
	}

	wq.incoming.In <- work
	return nil
}

func (wq *WorkQueue) doWork() {
	for {
		select {

		case work := <-wq.incoming.Out:
			select {
			// Writing to limiter will block if attempting to start more goroutines than concurrency level (semaphore semantics).
			case wq.limiter <- struct{}{}:
				go func() {
					defer func() { <-wq.limiter }()
					work(wq.lifetimeCtx)
				}()

			// We want to stop the worker goroutine if the lifetime context is done (cancel the wait on writing to limiter).
			case <-wq.lifetimeCtx.Done():
				return
			}

		case <-wq.lifetimeCtx.Done():
			return
		}

	}
}

func getDefaultConcurrency() uint8 {
	numCPU := runtime.NumCPU()
	if numCPU > math.MaxUint8 {
		return math.MaxUint8
	} else {
		return uint8(numCPU)
	}
}
