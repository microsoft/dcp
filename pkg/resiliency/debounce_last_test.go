/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExecutesRunnerAfterDelay(t *testing.T) {
	t.Parallel()

	runner := func() (int, error) {
		return 7, nil
	}

	const debounceDelay = time.Millisecond * 100
	const testTimeoutDelay = time.Millisecond * 1000

	deb := NewDebounceLast(runner, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	start := time.Now()
	res, err := deb.Run(ctx)
	finish := time.Now()

	require.NoError(t, err)
	require.Equal(t, 7, res)

	// The call should happen after debounceDelay
	require.WithinRange(t, finish, start.Add(debounceDelay), time.Now().Add(testTimeoutDelay))
}

func TestReturnsErrorFromRunner(t *testing.T) {
	t.Parallel()

	runner := func() (int, error) {
		return 0, fmt.Errorf("sorry")
	}

	const debounceDelay = time.Millisecond * 100
	const testTimeoutDelay = time.Millisecond * 1000

	deb := NewDebounceLast(runner, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	start := time.Now()
	_, err := deb.Run(ctx)
	finish := time.Now()

	require.Error(t, err)

	require.WithinRange(t, finish, start.Add(debounceDelay), time.Now().Add(testTimeoutDelay))
}

func TestDebouncesRapidInvocations(t *testing.T) {
	t.Parallel()

	runnerCalls := atomic.Int32{}
	runner := func() (int, error) {
		return int(runnerCalls.Add(1)), nil
	}

	const debounceDelay = time.Millisecond * 200
	const testTimeoutDelay = time.Millisecond * 1000

	deb := NewDebounceLast(runner, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	const numCalls = 5

	results := make([]int, numCalls)
	wg := sync.WaitGroup{}
	wg.Add(numCalls)

	start := time.Now()
	// Make numCalls calls to runner as fast as possible.
	for i := 0; i < numCalls; i++ {
		go func(index int) {
			res, err := deb.Run(ctx)
			require.NoError(t, err)
			results[index] = res
			wg.Done()
		}(i)
	}

	wg.Wait()
	finish := time.Now()

	// The results should all be the same
	require.IsNonIncreasing(t, results)
	require.IsNonDecreasing(t, results)

	require.Equal(t, 1, results[0])
	require.Equal(t, int32(1), runnerCalls.Load())

	require.WithinRange(t, finish, start.Add(debounceDelay), time.Now().Add(testTimeoutDelay))
}

func TestDebounceIsReusable(t *testing.T) {
	t.Parallel()

	runnerCalls := atomic.Int32{}
	runner := func() (int32, error) {
		runnerCalls.Add(1)
		return 1, nil
	}

	const debounceDelay = time.Millisecond * 150
	const testTimeoutDelay = time.Millisecond * 1000
	const numCalls = 3

	deb := NewDebounceLast(runner, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	wg := sync.WaitGroup{}
	var sum int32

	makeCalls := func() {
		wg.Add(numCalls)

		for i := 0; i < numCalls; i++ {
			go func() {
				res, err := deb.Run(ctx)
				require.NoError(t, err)
				atomic.AddInt32(&sum, res)
				wg.Done()
			}()
		}

		wg.Wait()
	}

	start := time.Now()
	testTimeout := start.Add(testTimeoutDelay)
	makeCalls()
	finish := time.Now()

	// Verify all calls were made and the results add up to expected value
	require.WithinRange(t, finish, start.Add(debounceDelay), testTimeout)
	require.Equal(t, int32(numCalls), sum)
	require.Equal(t, int32(1), runnerCalls.Load())

	// Now the same debounce should be ready for another round of calls
	sum = 0

	start = time.Now()
	makeCalls()
	finish = time.Now()

	require.WithinRange(t, finish, start.Add(debounceDelay), testTimeout)
	require.Equal(t, int32(numCalls), sum)
	require.Equal(t, int32(2), runnerCalls.Load())
}

func TestRunDuringRunnerExecutionStartsNewRun(t *testing.T) {
	t.Parallel()

	const debounceDelay = time.Millisecond * 500
	const testTimeoutDelay = time.Second * 5

	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	runnerCalls := atomic.Int32{}

	runner := func() (int32, error) {
		call := runnerCalls.Add(1)

		switch call {
		case 1:
			close(firstStarted)
			<-releaseFirst
			return 1, nil
		case 2:
			close(secondStarted)
			<-releaseSecond
			return 2, nil
		default:
			return 0, fmt.Errorf("unexpected runner call: %d", call)
		}
	}

	deb := NewDebounceLast(runner, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	firstResultC := make(chan ResultWithError[int32], 1)
	secondResultC := make(chan ResultWithError[int32], 1)

	go func() {
		v, err := deb.Run(ctx)
		firstResultC <- ResultWithError[int32]{V: v, Err: err}
	}()

	select {
	case <-firstStarted:
	case <-time.After(testTimeoutDelay):
		require.FailNow(t, "first runner call did not start in time")
	}

	go func() {
		v, err := deb.Run(ctx)
		secondResultC <- ResultWithError[int32]{V: v, Err: err}
	}()

	select {
	case <-secondStarted:
	case <-time.After(testTimeoutDelay):
		require.FailNow(t, "second runner call did not start as a new run")
	}

	close(releaseFirst)
	firstResult := <-firstResultC
	require.NoError(t, firstResult.Err)
	require.Equal(t, int32(1), firstResult.V)

	select {
	case secondResult := <-secondResultC:
		require.FailNowf(t, "second run should still be waiting", "unexpected result: %+v", secondResult)
	case <-time.After(debounceDelay):
	}

	close(releaseSecond)
	secondResult := <-secondResultC
	require.NoError(t, secondResult.Err)
	require.Equal(t, int32(2), secondResult.V)
	require.Equal(t, int32(2), runnerCalls.Load())
}

func TestReturnsErrorIfContextCancelled(t *testing.T) {
	t.Parallel()

	runner := func() (int, error) {
		return 7, nil
	}

	const debounceDelay = time.Millisecond * 500
	const contextTimeoutDelay = time.Millisecond * 100

	deb := NewDebounceLast(runner, debounceDelay, 2*time.Second)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeoutDelay)
	defer cancel()
	_, err := deb.Run(ctx)
	finish := time.Now()

	require.ErrorIs(t, err, context.DeadlineExceeded)
	// Assuming debounceDelay is significantly larger than contextTimeoutDelay, the call should return
	// before debounceDelay.
	require.WithinRange(t, finish, start.Add(contextTimeoutDelay), start.Add(debounceDelay))
}
