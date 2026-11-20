/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that PartitionedWorkQueue with UnboundedConcurrency bounds concurrency independently per partition
// so stalled work on one partition never blocks other partitions.
func TestPartitionedWorkQueueIsolatesPartitionsWhenTotalUnbounded(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	const perPartitionLimit = 2
	pwq := resiliency.NewPartitionedWorkQueue[string](ctx, perPartitionLimit, resiliency.UnboundedConcurrency)
	require.Error(t, pwq.Enqueue("invalid", nil))

	releaseStalled := make(chan struct{})
	defer func() {
		select {
		case <-releaseStalled:
		default:
			close(releaseStalled)
		}
	}()

	var stalledRunning, stalledPeak atomic.Int32
	stalledSaturated := make(chan struct{})
	var saturatedOnce sync.Once
	var stalledWG sync.WaitGroup

	for range 6 {
		stalledWG.Add(1)
		require.NoError(t, pwq.Enqueue("stalled", func(workCtx context.Context) {
			defer stalledWG.Done()
			current := stalledRunning.Add(1)
			for {
				peak := stalledPeak.Load()
				if current <= peak || stalledPeak.CompareAndSwap(peak, current) {
					break
				}
			}
			if current == perPartitionLimit {
				saturatedOnce.Do(func() { close(stalledSaturated) })
			}
			select {
			case <-releaseStalled:
			case <-workCtx.Done():
			}
			stalledRunning.Add(-1)
		}))
	}

	select {
	case <-stalledSaturated:
	case <-ctx.Done():
		t.Fatal("Stalled partition did not reach its concurrency limit")
	}

	var fastCompleted atomic.Uint32
	var fastWG sync.WaitGroup
	for partitionIdx := range 8 {
		partition := string(rune('a' + partitionIdx))
		for range 4 {
			fastWG.Add(1)
			require.NoError(t, pwq.Enqueue(partition, func(context.Context) {
				defer fastWG.Done()
				fastCompleted.Add(1)
			}))
		}
	}
	fastWG.Wait()
	require.Equal(t, uint32(32), fastCompleted.Load())

	close(releaseStalled)
	stalledWG.Wait()
	require.Equal(t, int32(perPartitionLimit), stalledPeak.Load())
}

// Verifies that PartitionedWorkQueue enforces both maxPartitionConcurrency and nonzero maxTotalConcurrency
// while scheduling ready partitions fairly in round-robin order.
func TestPartitionedWorkQueueEnforcesTotalConcurrencyAndRoundRobinFairness(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	fairQueue := resiliency.NewPartitionedWorkQueue[string](ctx, 1, 1)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	events := make(chan string, 5)

	require.NoError(t, fairQueue.Enqueue("A", func(context.Context) {
		close(firstStarted)
		<-releaseFirst
		events <- "A1"
	}))
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("First item on partition A did not start")
	}

	require.NoError(t, fairQueue.Enqueue("A", func(context.Context) { events <- "A2" }))
	require.NoError(t, fairQueue.Enqueue("A", func(context.Context) { events <- "A3" }))
	require.NoError(t, fairQueue.Enqueue("B", func(context.Context) { events <- "B1" }))

	close(releaseFirst)
	var observed []string
	for range 4 {
		select {
		case evt := <-events:
			observed = append(observed, evt)
		case <-ctx.Done():
			t.Fatal("Timed out waiting for fair round-robin execution")
		}
	}
	require.Equal(t, []string{"A1", "B1", "A2", "A3"}, observed)

	const maxPerPartition = 2
	const maxTotal = 3
	cappedQueue := resiliency.NewPartitionedWorkQueue[int](ctx, maxPerPartition, maxTotal)

	var totalActive, totalPeak atomic.Int32
	var partitionActive [4]atomic.Int32
	var partitionPeak [4]atomic.Int32
	recordPeak := func(target *atomic.Int32, value int32) {
		for {
			peak := target.Load()
			if value <= peak || target.CompareAndSwap(peak, value) {
				return
			}
		}
	}

	// Deterministically saturate maxTotal across distinct partitions and verify additional queued items do not exceed it.
	totalSaturated := make(chan struct{})
	releaseTotal := make(chan struct{})
	var totalSaturatedOnce sync.Once
	var closeReleaseTotalOnce sync.Once
	defer closeReleaseTotalOnce.Do(func() { close(releaseTotal) })

	var totalWG sync.WaitGroup
	for p := range 4 {
		partition := p
		for range 2 {
			totalWG.Add(1)
			require.NoError(t, cappedQueue.Enqueue(partition, func(workCtx context.Context) {
				defer totalWG.Done()
				curTotal := totalActive.Add(1)
				recordPeak(&totalPeak, curTotal)
				curPart := partitionActive[partition].Add(1)
				recordPeak(&partitionPeak[partition], curPart)
				if curTotal == maxTotal {
					totalSaturatedOnce.Do(func() { close(totalSaturated) })
				}
				select {
				case <-releaseTotal:
				case <-workCtx.Done():
				}
				partitionActive[partition].Add(-1)
				totalActive.Add(-1)
			}))
		}
	}

	select {
	case <-totalSaturated:
	case <-ctx.Done():
		t.Fatal("Queue did not reach maxTotal concurrency")
	}
	closeReleaseTotalOnce.Do(func() { close(releaseTotal) })
	totalWG.Wait()
	require.Equal(t, int32(maxTotal), totalPeak.Load())
	for p := range 4 {
		require.LessOrEqual(t, partitionPeak[p].Load(), int32(maxPerPartition))
	}

	// Deterministically saturate maxPerPartition on a single partition while global capacity remains available.
	partitionSaturated := make(chan struct{})
	releasePartition := make(chan struct{})
	var partitionSaturatedOnce sync.Once
	var closeReleasePartitionOnce sync.Once
	defer closeReleasePartitionOnce.Do(func() { close(releasePartition) })

	partitionPeak[0].Store(0)
	totalPeak.Store(0)
	var partWG sync.WaitGroup
	for range 4 {
		partWG.Add(1)
		require.NoError(t, cappedQueue.Enqueue(0, func(workCtx context.Context) {
			defer partWG.Done()
			curTotal := totalActive.Add(1)
			recordPeak(&totalPeak, curTotal)
			curPart := partitionActive[0].Add(1)
			recordPeak(&partitionPeak[0], curPart)
			if curPart == maxPerPartition {
				partitionSaturatedOnce.Do(func() { close(partitionSaturated) })
			}
			select {
			case <-releasePartition:
			case <-workCtx.Done():
			}
			partitionActive[0].Add(-1)
			totalActive.Add(-1)
		}))
	}

	select {
	case <-partitionSaturated:
	case <-ctx.Done():
		t.Fatal("Partition 0 did not reach maxPerPartition concurrency")
	}
	closeReleasePartitionOnce.Do(func() { close(releasePartition) })
	partWG.Wait()
	require.Equal(t, int32(maxPerPartition), partitionPeak[0].Load())
	require.Equal(t, int32(maxPerPartition), totalPeak.Load())
}

// Verifies that PartitionedWorkQueue releases its partition and total concurrency slots when
// a work item terminates its goroutine via runtime.Goexit, allowing queued items to execute.
func TestPartitionedWorkQueueReleasesSlotOnGoexit(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	pwq := resiliency.NewPartitionedWorkQueue[string](ctx, 1, 1)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondCompleted := make(chan struct{})

	require.NoError(t, pwq.Enqueue("partition", func(workCtx context.Context) {
		close(firstStarted)
		select {
		case <-releaseFirst:
		case <-workCtx.Done():
			return
		}
		runtime.Goexit()
	}))
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("First work item did not start")
	}

	require.NoError(t, pwq.Enqueue("partition", func(context.Context) {
		close(secondCompleted)
	}))
	close(releaseFirst)

	select {
	case <-secondCompleted:
	case <-ctx.Done():
		t.Fatal("Queued work item did not execute after predecessor called runtime.Goexit")
	}

	for {
		partitions, running, pending := resiliency.InspectPartitionedQueueStateForTest(pwq)
		if partitions == 0 && running == 0 && pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Queue retained state after runtime.Goexit (partitions=%d, running=%d, pending=%d)", partitions, running, pending)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

// Verifies that PartitionedWorkQueue evicts idle partition entries after work drains, preserves per-partition
// concurrency bounds under concurrent reuse racing with eviction, and cleans up pending state on cancellation.
func TestPartitionedWorkQueueEvictsIdleAndCanceledPartitions(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()
	lifetimeCtx, cancelLifetime := context.WithCancel(ctx)
	defer cancelLifetime()

	pwq := resiliency.NewPartitionedWorkQueue[int](lifetimeCtx, 1, resiliency.UnboundedConcurrency)
	partitions, running, pending := resiliency.InspectPartitionedQueueStateForTest(pwq)
	require.Equal(t, 0, partitions)
	require.Equal(t, uint(0), running)
	require.Equal(t, uint(0), pending)

	var activeOnPartition atomic.Int32
	var maxActiveOnPartition atomic.Int32
	var completed atomic.Uint32

	var submitWG sync.WaitGroup
	for range 16 {
		submitWG.Go(func() {
			for range 25 {
				require.NoError(t, pwq.Enqueue(7, func(context.Context) {
					current := activeOnPartition.Add(1)
					for {
						peak := maxActiveOnPartition.Load()
						if current <= peak || maxActiveOnPartition.CompareAndSwap(peak, current) {
							break
						}
					}
					activeOnPartition.Add(-1)
					completed.Add(1)
				}))
			}
		})
	}
	submitWG.Wait()

	for {
		partitions, running, pending = resiliency.InspectPartitionedQueueStateForTest(pwq)
		if completed.Load() == 16*25 && partitions == 0 && running == 0 && pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Timed out waiting for partition eviction (completed=%d, partitions=%d, running=%d, pending=%d)",
				completed.Load(), partitions, running, pending)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	require.Equal(t, int32(1), maxActiveOnPartition.Load())

	started := make(chan struct{})
	release := make(chan struct{})
	var droppedExecuted atomic.Bool

	require.NoError(t, pwq.Enqueue(1, func(context.Context) {
		close(started)
		<-release
	}))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Blocked item did not start")
	}
	require.NoError(t, pwq.Enqueue(1, func(context.Context) {
		droppedExecuted.Store(true)
	}))

	cancelLifetime()
	for {
		_, _, pending = resiliency.InspectPartitionedQueueStateForTest(pwq)
		if pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Pending items were not discarded after lifetime cancellation")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	close(release)
	for {
		partitions, running, pending = resiliency.InspectPartitionedQueueStateForTest(pwq)
		if partitions == 0 && running == 0 && pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Canceled queue retained state (partitions=%d, running=%d, pending=%d)", partitions, running, pending)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	require.False(t, droppedExecuted.Load())
	require.ErrorIs(t, pwq.Enqueue(1, func(context.Context) {}), context.Canceled)
}
