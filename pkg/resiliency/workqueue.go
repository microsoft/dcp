/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

import (
	"context"
	"math"
	"runtime"
)

const DefaultConcurrency uint8 = 0

type WorkQueueItem = func(ctx context.Context)

// WorkQueue runs work concurrently, but limits the number of concurrent executions.
//
// The queue is bound to a lifetime context. Once that context is done the queue stops
// draining and any items still queued are dropped without running, so callers that rely on
// queued work completing (for example resource cleanup on shutdown) must do so before the
// lifetime context is cancelled.
type WorkQueue struct {
	inner *PartitionedWorkQueue[struct{}]
}

func NewWorkQueue(lifetimeCtx context.Context, maxConcurrency uint8) *WorkQueue {
	return &WorkQueue{
		inner: NewPartitionedWorkQueue[struct{}](lifetimeCtx, maxConcurrency, UnboundedConcurrency),
	}
}

// Queues a work item to be executed. If the lifetime context is done, returns an error.
func (wq *WorkQueue) Enqueue(work WorkQueueItem) error {
	return wq.inner.Enqueue(struct{}{}, work)
}

func getDefaultConcurrency() uint8 {
	numCPU := runtime.NumCPU()
	if numCPU > math.MaxUint8 {
		return math.MaxUint8
	} else {
		return uint8(numCPU)
	}
}
