/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ForEachBounded invokes action for each item with at most maxConcurrency concurrent calls.
// Processing stops after the first error or context cancellation.
func ForEachBounded[T any](ctx context.Context, items []T, maxConcurrency int, action func(context.Context, T) error) error {
	if maxConcurrency <= 0 {
		return fmt.Errorf("max concurrency must be greater than zero")
	}
	if action == nil {
		return errors.New("action must not be nil")
	}
	if len(items) == 0 {
		return nil
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()

	jobs := make(chan T)
	workerCount := min(maxConcurrency, len(items))
	workers := sync.WaitGroup{}
	workers.Add(workerCount)

	var firstActionErr error
	firstActionErrOnce := sync.Once{}
	for range workerCount {
		go func() {
			defer workers.Done()

			for {
				select {
				case <-workerCtx.Done():
					return
				case item, ok := <-jobs:
					if !ok {
						return
					}
					actionErr := action(workerCtx, item)
					if actionErr != nil {
						firstActionErrOnce.Do(func() {
							firstActionErr = actionErr
							cancelWorkers()
						})
						return
					}
				}
			}
		}()
	}

sendItems:
	for _, item := range items {
		select {
		case <-workerCtx.Done():
			break sendItems
		case jobs <- item:
		}
	}
	close(jobs)
	workers.Wait()

	if firstActionErr != nil {
		return firstActionErr
	}
	return ctx.Err()
}
