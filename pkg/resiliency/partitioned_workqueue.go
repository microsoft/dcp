/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

import (
	"context"
	"errors"
	"sync"

	"github.com/microsoft/dcp/pkg/container"
)

// UnboundedConcurrency indicates no cross-partition cap on total concurrent executions in PartitionedWorkQueue.
const UnboundedConcurrency uint = 0

type partitionState struct {
	pending   *container.RingBuffer[WorkQueueItem]
	running   uint8
	scheduled bool // True while this partition is queued in readyPartitions.
}

// PartitionedWorkQueue runs work concurrently while bounding active executions per partition
// and optionally across all partitions combined.
//
// Worker goroutines are started on demand and exit as soon as queued work drains, and idle
// partitions retain no map entries or goroutines. Once lifetimeCtx is done, new submissions
// are rejected and any items still queued are dropped without running.
type PartitionedWorkQueue[P comparable] struct {
	lifetimeCtx             context.Context
	maxPartitionConcurrency uint8
	maxTotalConcurrency     uint

	lock              sync.Mutex
	partitions        map[P]*partitionState
	readyPartitions   *container.RingBuffer[P]
	totalRunning      uint
	pendingCount      uint
	stopCancelCleanup func() bool
}

// NewPartitionedWorkQueue creates a PartitionedWorkQueue bound to lifetimeCtx.
// If maxPartitionConcurrency is DefaultConcurrency (0), DefaultConcurrency is resolved to the CPU count.
// If maxTotalConcurrency is UnboundedConcurrency (0), total concurrency across partitions is unbounded;
// otherwise at most maxTotalConcurrency items execute simultaneously across all partitions, scheduled
// fairly in round-robin order among runnable partitions.
func NewPartitionedWorkQueue[P comparable](
	lifetimeCtx context.Context,
	maxPartitionConcurrency uint8,
	maxTotalConcurrency uint,
) *PartitionedWorkQueue[P] {
	if maxPartitionConcurrency == DefaultConcurrency {
		maxPartitionConcurrency = getDefaultConcurrency()
	}

	return &PartitionedWorkQueue[P]{
		lifetimeCtx:             lifetimeCtx,
		maxPartitionConcurrency: maxPartitionConcurrency,
		maxTotalConcurrency:     maxTotalConcurrency,
		partitions:              make(map[P]*partitionState),
		readyPartitions:         container.NewRingBuffer[P](),
	}
}

// Enqueue schedules work within the given concurrency partition.
// Returns an error if work is nil or if lifetimeCtx is done.
func (pwq *PartitionedWorkQueue[P]) Enqueue(partition P, work WorkQueueItem) error {
	if work == nil {
		return errors.New("work item cannot be nil")
	}

	pwq.lock.Lock()
	if ctxErr := pwq.lifetimeCtx.Err(); ctxErr != nil {
		pwq.lock.Unlock()
		return ctxErr
	}

	ps, exists := pwq.partitions[partition]
	hasGlobalCapacity := pwq.maxTotalConcurrency == UnboundedConcurrency || pwq.totalRunning < pwq.maxTotalConcurrency
	if (ps == nil || (ps.pending.Empty() && ps.running < pwq.maxPartitionConcurrency)) && hasGlobalCapacity {
		if !exists {
			ps = &partitionState{
				pending: container.NewRingBuffer[WorkQueueItem](),
			}
			pwq.partitions[partition] = ps
		}
		ps.running++
		pwq.totalRunning++
		pwq.lock.Unlock()

		go pwq.runWorker(partition, ps, work)
		return nil
	}

	if !exists {
		ps = &partitionState{
			pending: container.NewRingBuffer[WorkQueueItem](),
		}
		pwq.partitions[partition] = ps
	}
	ps.pending.Push(work)
	pwq.pendingCount++
	if pwq.stopCancelCleanup == nil {
		pwq.stopCancelCleanup = context.AfterFunc(pwq.lifetimeCtx, pwq.discardPending)
	}
	if !ps.scheduled && ps.running < pwq.maxPartitionConcurrency {
		ps.scheduled = true
		pwq.readyPartitions.Push(partition)
	}
	pwq.lock.Unlock()
	return nil
}

func (pwq *PartitionedWorkQueue[P]) runWorker(initialPartition P, initialState *partitionState, initialWork WorkQueueItem) {
	currentPartition := initialPartition
	currentState := initialState
	currentWork := initialWork

	for {
		completedNormally := false
		func() {
			defer func() {
				if !completedNormally {
					nextPartition, nextState, nextWork, hasNext := pwq.advanceWorker(currentPartition, currentState)
					if hasNext {
						go pwq.runWorker(nextPartition, nextState, nextWork)
					}
				}
			}()
			currentWork(pwq.lifetimeCtx)
			completedNormally = true
		}()

		var hasNext bool
		currentPartition, currentState, currentWork, hasNext = pwq.advanceWorker(currentPartition, currentState)
		if !hasNext {
			return
		}
	}
}

func (pwq *PartitionedWorkQueue[P]) advanceWorker(
	currentPartition P,
	currentState *partitionState,
) (P, *partitionState, WorkQueueItem, bool) {
	var zeroPartition P

	pwq.lock.Lock()
	defer pwq.lock.Unlock()

	if pwq.lifetimeCtx.Err() != nil {
		pwq.clearPendingLocked()
		pwq.completeWorkItemLocked(currentPartition, currentState)
		pwq.totalRunning--
		return zeroPartition, nil, nil, false
	}

	pwq.completeWorkItemLocked(currentPartition, currentState)
	nextPartition, nextState, nextWork, hasNext := pwq.takeNextWorkItemLocked()
	if !hasNext {
		pwq.totalRunning--
		return zeroPartition, nil, nil, false
	}
	return nextPartition, nextState, nextWork, true
}

func (pwq *PartitionedWorkQueue[P]) completeWorkItemLocked(partition P, state *partitionState) {
	state.running--
	if !state.pending.Empty() {
		if !state.scheduled && state.running < pwq.maxPartitionConcurrency {
			state.scheduled = true
			pwq.readyPartitions.Push(partition)
		}
	} else if state.running == 0 {
		delete(pwq.partitions, partition)
	}
}

func (pwq *PartitionedWorkQueue[P]) takeNextWorkItemLocked() (P, *partitionState, WorkQueueItem, bool) {
	var zeroPartition P

	nextPartition, hasReady := pwq.readyPartitions.Pop()
	if !hasReady {
		return zeroPartition, nil, nil, false
	}

	nextState := pwq.partitions[nextPartition]
	nextState.scheduled = false
	nextWork, foundWork := nextState.pending.Pop()
	if !foundWork {
		if nextState.running == 0 {
			delete(pwq.partitions, nextPartition)
		}
		return zeroPartition, nil, nil, false
	}
	pwq.pendingCount--
	if pwq.pendingCount == 0 && pwq.stopCancelCleanup != nil {
		pwq.stopCancelCleanup()
		pwq.stopCancelCleanup = nil
	}
	nextState.running++
	if !nextState.pending.Empty() && nextState.running < pwq.maxPartitionConcurrency {
		nextState.scheduled = true
		pwq.readyPartitions.Push(nextPartition)
	}
	return nextPartition, nextState, nextWork, true
}

func (pwq *PartitionedWorkQueue[P]) discardPending() {
	pwq.lock.Lock()
	defer pwq.lock.Unlock()
	pwq.stopCancelCleanup = nil
	pwq.clearPendingLocked()
}

func (pwq *PartitionedWorkQueue[P]) clearPendingLocked() {
	if pwq.readyPartitions.Len() > 0 {
		pwq.readyPartitions = container.NewRingBuffer[P]()
	}
	for partition, ps := range pwq.partitions {
		ps.scheduled = false
		if ps.running == 0 {
			delete(pwq.partitions, partition)
		} else if !ps.pending.Empty() {
			ps.pending = container.NewRingBuffer[WorkQueueItem]()
		}
	}
	pwq.pendingCount = 0
	if pwq.stopCancelCleanup != nil {
		pwq.stopCancelCleanup()
		pwq.stopCancelCleanup = nil
	}
}
