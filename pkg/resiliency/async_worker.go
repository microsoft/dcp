/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

import (
	"context"

	"github.com/microsoft/dcp/pkg/concurrency"
)

// Async worker is a worker that runs asynchronously, processing items from a work queue
// and returning results through a channel, with limited concurrency.
type AsyncWorker[InputT, OutputT any] struct {
	lifetimeCtx context.Context
	wq          *WorkQueue
	results     *concurrency.UnboundedChan[OutputT]
	getResult   func(context.Context, InputT) OutputT
}

func NewAsyncWorker[InputT, OutputT any](
	lifetimeCtx context.Context,
	getResult func(context.Context, InputT) OutputT,
	maxConcurrency uint8,
) *AsyncWorker[InputT, OutputT] {
	wq := NewWorkQueue(lifetimeCtx, maxConcurrency)
	results := concurrency.NewUnboundedChan[OutputT](lifetimeCtx)

	return &AsyncWorker[InputT, OutputT]{
		lifetimeCtx: lifetimeCtx,
		wq:          wq,
		results:     results,
		getResult:   getResult,
	}
}

func (aw *AsyncWorker[InputT, OutputT]) Enqueue(input InputT) error {
	if aw.lifetimeCtx.Err() != nil {
		return aw.lifetimeCtx.Err()
	}

	workItem := func(ctx context.Context) {
		result := aw.getResult(ctx, input)
		aw.results.In <- result
	}

	return aw.wq.Enqueue(workItem)
}

func (aw *AsyncWorker[InputT, OutputT]) Results() <-chan OutputT {
	return aw.results.Out
}
