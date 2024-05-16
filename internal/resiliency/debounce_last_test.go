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

	runner := func(i int) (int, error) {
		return i, nil
	}

	const debounceDelay = time.Millisecond * 100
	const testTimeoutDelay = time.Millisecond * 1000

	deb := NewDebounceLast(runner, debounceDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	start := time.Now()
	res, err := deb.Run(ctx, 7)
	finish := time.Now()

	require.NoError(t, err)
	require.Equal(t, 7, res)

	// The call should happen after debounceDelay
	require.WithinRange(t, finish, start.Add(debounceDelay), time.Now().Add(testTimeoutDelay))
}

func TestReturnsErrorFromRunner(t *testing.T) {
	t.Parallel()

	runner := func(i int) (int, error) {
		return 0, fmt.Errorf("Sorry")
	}

	const debounceDelay = time.Millisecond * 100
	const testTimeoutDelay = time.Millisecond * 1000

	deb := NewDebounceLast(runner, debounceDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	start := time.Now()
	_, err := deb.Run(ctx, 7)
	finish := time.Now()

	require.Error(t, err)

	require.WithinRange(t, finish, start.Add(debounceDelay), time.Now().Add(testTimeoutDelay))
}

func TestDebouncesRapidInvocations(t *testing.T) {
	t.Parallel()

	runner := func(i int) (int, error) {
		return i, nil
	}

	const debounceDelay = time.Millisecond * 200
	const testTimeoutDelay = time.Millisecond * 1000

	deb := NewDebounceLast(runner, debounceDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	const resultShift = 10
	const numCalls = 5

	results := make([]int, numCalls)
	wg := sync.WaitGroup{}
	wg.Add(numCalls)

	start := time.Now()
	// Make numCalls calls to runner, in order, as fast as possible
	for i := 0; i < numCalls; i++ {
		go func(j int) {
			res, err := deb.Run(ctx, j)
			require.NoError(t, err)
			results[j-resultShift] = res
			wg.Done()
		}(i + resultShift)
	}

	wg.Wait()
	finish := time.Now()

	// The results should all be the same
	require.IsNonIncreasing(t, results)
	require.IsNonDecreasing(t, results)

	// It is not predictable what the result will be, because the order of goroutines executing the call
	// is random, but they should be within [resultShift, resultShift + numCalls).
	require.GreaterOrEqual(t, results[0], resultShift)
	require.Less(t, results[0], resultShift+numCalls)

	require.WithinRange(t, finish, start.Add(debounceDelay), time.Now().Add(testTimeoutDelay))
}

func TestDebounceIsReusable(t *testing.T) {
	t.Parallel()

	runner := func(i int32) (int32, error) {
		return i, nil
	}

	const debounceDelay = time.Millisecond * 150
	const testTimeoutDelay = time.Millisecond * 1000
	const numCalls = 3

	deb := NewDebounceLast(runner, debounceDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	wg := sync.WaitGroup{}
	var sum int32

	makeCalls := func() {
		wg.Add(numCalls)

		for i := 0; i < numCalls; i++ {
			go func() {
				res, err := deb.Run(ctx, 1)
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

	// Now the same debounce should be ready for another round of calls
	const secondRunResultShift = 10
	sum = secondRunResultShift

	start = time.Now()
	makeCalls()
	finish = time.Now()

	require.WithinRange(t, finish, start.Add(debounceDelay), testTimeout)
	require.Equal(t, int32(numCalls+secondRunResultShift), sum)
}

func TestReturnsErrorIfContextCancelled(t *testing.T) {
	t.Parallel()

	runner := func(i int) (int, error) {
		return i, nil
	}

	const debounceDelay = time.Millisecond * 500
	const contextTimeoutDelay = time.Millisecond * 100

	deb := NewDebounceLast(runner, debounceDelay)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeoutDelay)
	defer cancel()
	_, err := deb.Run(ctx, 7)
	finish := time.Now()

	require.ErrorIs(t, err, context.DeadlineExceeded)
	// Assuming debounceDelay is significantly larger than contextTimeoutDelay, the call should return
	// before debounceDelay.
	require.WithinRange(t, finish, start.Add(contextTimeoutDelay), start.Add(debounceDelay))
}
