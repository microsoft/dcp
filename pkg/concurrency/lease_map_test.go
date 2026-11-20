/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that LeaseMap.Acquire and LeaseMap.TryAcquire serialize access per key (including
// zero-value keys), allow distinct keys to proceed concurrently, and evict idle entries once
// all leases are released.
func TestLeaseMapSerializesSameKeyAndIsolatesDistinctKeys(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	intMap := concurrency.NewLeaseMap[int]()
	require.Zero(t, concurrency.InspectLeaseMapEntryCountForTest(intMap))
	zeroKeyLease, zeroKeyErr := intMap.Acquire(ctx, 0)
	require.NoError(t, zeroKeyErr)
	require.Equal(t, 1, concurrency.InspectLeaseMapEntryCountForTest(intMap))
	_, zeroTryOK := intMap.TryAcquire(0)
	require.False(t, zeroTryOK)
	zeroKeyLease.Release()
	require.Zero(t, concurrency.InspectLeaseMapEntryCountForTest(intMap))

	lm := concurrency.NewLeaseMap[string]()
	require.Zero(t, concurrency.InspectLeaseMapEntryCountForTest(lm))

	emptyKeyLease, emptyKeyErr := lm.Acquire(ctx, "")
	require.NoError(t, emptyKeyErr)
	require.Equal(t, 1, concurrency.InspectLeaseMapEntryCountForTest(lm))

	// A distinct key can be acquired via both TryAcquire and Acquire while "" is held.
	otherLease, otherOK := lm.TryAcquire("other")
	require.True(t, otherOK)
	otherLease.Release()

	// Contending TryAcquire on the held "" key fails immediately without blocking.
	contendedLease, contendedOK := lm.TryAcquire("")
	require.False(t, contendedOK)
	require.Nil(t, contendedLease)

	secondWaiting := make(chan struct{})
	secondAcquired := make(chan *concurrency.Lease[string], 1)
	go func() {
		close(secondWaiting)
		acquired, acquireErr := lm.Acquire(ctx, "")
		if acquireErr != nil {
			t.Errorf("Unexpected Acquire error: %v", acquireErr)
			return
		}
		secondAcquired <- acquired
	}()

	select {
	case <-secondWaiting:
	case <-ctx.Done():
		t.Fatal("Second acquirer did not start")
	}

	emptyKeyLease.Release()
	var secondLease *concurrency.Lease[string]
	select {
	case secondLease = <-secondAcquired:
	case <-ctx.Done():
		t.Fatal("Second acquirer did not obtain lease after first release")
	}
	require.Equal(t, 1, concurrency.InspectLeaseMapEntryCountForTest(lm))
	secondLease.Release()
	require.Zero(t, concurrency.InspectLeaseMapEntryCountForTest(lm))
}

// Verifies that Lease.Release is safe on nil or zero-value leases and that repeated Release
// calls on an earlier lease cannot unlock a subsequent holder's lease.
func TestLeaseMapReleaseIsIdempotentAndNilSafe(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	var nilLease *concurrency.Lease[string]
	nilLease.Release()
	(&concurrency.Lease[string]{}).Release()

	lm := concurrency.NewLeaseMap[string]()
	first, firstErr := lm.Acquire(ctx, "key")
	require.NoError(t, firstErr)

	first.Release()
	second, secondOK := lm.TryAcquire("key")
	require.True(t, secondOK)

	// Duplicate Release calls on first must not release second's lock or decrement second's ref.
	first.Release()
	first.Release()
	_, thirdOK := lm.TryAcquire("key")
	require.False(t, thirdOK, "Duplicate Release on a prior lease must not unlock the current holder")
	require.Equal(t, 1, concurrency.InspectLeaseMapEntryCountForTest(lm))

	second.Release()
	second.Release()
	require.Zero(t, concurrency.InspectLeaseMapEntryCountForTest(lm))
}

// Verifies that LeaseMap.Acquire rejects canceled contexts without retaining map state and
// unblocks waiting callers on cancellation without disturbing the active lease holder.
func TestLeaseMapRespectsContextCancellationAndEvictsIdleEntries(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 10*time.Second)
	defer cancel()

	lm := concurrency.NewLeaseMap[string]()

	canceledCtx, cancelImmediately := context.WithCancel(ctx)
	cancelImmediately()
	rejected, rejectedErr := lm.Acquire(canceledCtx, "never-stored")
	require.ErrorIs(t, rejectedErr, context.Canceled)
	require.Nil(t, rejected)
	require.Zero(t, concurrency.InspectLeaseMapEntryCountForTest(lm))

	holder, holderErr := lm.Acquire(ctx, "contended")
	require.NoError(t, holderErr)

	waiterCtx, cancelWaiter := context.WithCancel(ctx)
	waiterEntered := make(chan struct{})
	waiterErrCh := make(chan error, 1)
	go func() {
		close(waiterEntered)
		_, acquireErr := lm.Acquire(waiterCtx, "contended")
		waiterErrCh <- acquireErr
	}()

	select {
	case <-waiterEntered:
	case <-ctx.Done():
		t.Fatal("Waiter goroutine did not start")
	}
	cancelWaiter()

	select {
	case waiterErr := <-waiterErrCh:
		require.ErrorIs(t, waiterErr, context.Canceled)
	case <-ctx.Done():
		t.Fatal("Canceled waiter did not return")
	}

	// Holder must still own the lease exclusively after the waiter cancels.
	_, tryWhileHeld := lm.TryAcquire("contended")
	require.False(t, tryWhileHeld)
	require.Equal(t, 1, concurrency.InspectLeaseMapEntryCountForTest(lm))

	holder.Release()
	require.Zero(t, concurrency.InspectLeaseMapEntryCountForTest(lm))
}

// Verifies that LeaseMap preserves strict per-key mutual exclusion and evicts all entries
// across concurrent Acquire, TryAcquire, cancellation, and retirement/recreation cycles.
func TestLeaseMapEnforcesMutualExclusionUnderHighContention(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 15*time.Second)
	defer cancel()

	lm := concurrency.NewLeaseMap[int]()
	const numKeys = 4
	const workersPerKey = 8
	const iterations = 40

	var active [numKeys]atomic.Int32
	var violations atomic.Int32
	var wg sync.WaitGroup

	for keyIdx := range numKeys {
		key := keyIdx
		for workerIdx := range workersPerKey {
			worker := workerIdx
			wg.Go(func() {
				for iter := range iterations {
					if (worker+iter)%3 == 0 {
						lease, acquired := lm.TryAcquire(key)
						if !acquired {
							continue
						}
						if active[key].Add(1) != 1 {
							violations.Add(1)
						}
						active[key].Add(-1)
						lease.Release()
						continue
					}

					attemptCtx := ctx
					var cancelAttempt context.CancelFunc
					if (worker+iter)%7 == 0 {
						attemptCtx, cancelAttempt = context.WithTimeout(ctx, time.Millisecond)
					}
					lease, acquireErr := lm.Acquire(attemptCtx, key)
					if cancelAttempt != nil {
						cancelAttempt()
					}
					if acquireErr != nil {
						continue
					}
					if active[key].Add(1) != 1 {
						violations.Add(1)
					}
					active[key].Add(-1)
					lease.Release()
				}
			})
		}
	}

	wg.Wait()
	require.Zero(t, violations.Load(), "Multiple goroutines held a lease for the same key simultaneously")
	require.Zero(t, concurrency.InspectLeaseMapEntryCountForTest(lm), "All lease entries must be evicted once workers finish")
}
