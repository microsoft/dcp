/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/resiliency"
)

// Verifies that DebounceLast executes its runner after the configured delay and returns its result.
func TestExecutesRunnerAfterDelay(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		calls := atomic.Int32{}
		deb := resiliency.NewDebounceLast(func() (int, error) { calls.Add(1); return 7, nil }, time.Second, 5*time.Second)
		started := time.Now()
		result, runErr := deb.Run(ctx)
		require.NoError(test, runErr)
		require.Equal(test, 7, result)
		require.EqualValues(test, 1, calls.Load())
		require.Equal(test, time.Second, time.Since(started))
	})
}

// Verifies that DebounceLast preserves the runner's error after the configured delay.
func TestReturnsErrorFromRunner(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		failure := errors.New("runner failure")
		deb := resiliency.NewDebounceLast(func() (int, error) { return 0, failure }, time.Second, 5*time.Second)
		started := time.Now()
		_, runErr := deb.Run(ctx)
		require.ErrorIs(test, runErr, failure)
		require.Equal(test, time.Second, time.Since(started))
	})
}

// Verifies that DebounceLast postpones execution after each rapid call and returns one shared result.
func TestDebouncesRapidInvocations(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		const numCalls = 5
		calls := atomic.Int32{}
		deb := resiliency.NewDebounceLast(func() (int, error) { return int(calls.Add(1)), nil }, time.Second, 5*time.Second)
		results := make(chan resiliency.ResultWithError[int], numCalls)
		for index := range numCalls {
			if index != 0 {
				time.Sleep(100 * time.Millisecond)
			}
			go func() {
				result, runErr := deb.Run(ctx)
				results <- resiliency.ResultWithError[int]{V: result, Err: runErr}
			}()
			synctest.Wait()
			require.Zero(test, calls.Load())
		}
		lastCall := time.Now()
		for range numCalls {
			select {
			case result := <-results:
				require.NoError(test, result.Err)
				require.Equal(test, 1, result.V)
			case <-ctx.Done():
				test.Fatal("debounced call did not complete")
			}
		}
		synctest.Wait()
		require.EqualValues(test, 1, calls.Load())
		require.Equal(test, time.Second, time.Since(lastCall))
	})
}

// Verifies that DebounceLast starts a fresh coalesced run after the previous run completes.
func TestDebounceIsReusable(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		const numCalls = 3
		calls := atomic.Int32{}
		deb := resiliency.NewDebounceLast(func() (int, error) { return int(calls.Add(1)), nil }, time.Second, 5*time.Second)
		for round := 1; round <= 2; round++ {
			started := time.Now()
			results := make(chan resiliency.ResultWithError[int], numCalls)
			for range numCalls {
				go func() {
					result, runErr := deb.Run(ctx)
					results <- resiliency.ResultWithError[int]{V: result, Err: runErr}
				}()
			}
			synctest.Wait()
			require.EqualValues(test, round-1, calls.Load())
			for range numCalls {
				select {
				case result := <-results:
					require.NoError(test, result.Err)
					require.Equal(test, round, result.V)
				case <-ctx.Done():
					test.Fatal("reused debounce did not complete")
				}
			}
			require.EqualValues(test, round, calls.Load())
			require.Equal(test, time.Second, time.Since(started))
		}
	})
}

// Verifies that a new DebounceLast run can execute while the prior runner remains blocked,
// and completing the first run does not complete the second run.
func TestRunDuringRunnerExecutionStartsNewRun(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
		releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
		calls := atomic.Int32{}
		deb := resiliency.NewDebounceLast(func() (int, error) {
			call := int(calls.Add(1))
			var release <-chan struct{}
			switch call {
			case 1:
				close(firstStarted)
				release = releaseFirst
			case 2:
				close(secondStarted)
				release = releaseSecond
			default:
				return 0, errors.New("unexpected runner invocation")
			}
			select {
			case <-release:
				return call, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}, time.Second, 5*time.Second)
		firstResult := make(chan resiliency.ResultWithError[int], 1)
		secondResult := make(chan resiliency.ResultWithError[int], 1)
		go func() {
			result, runErr := deb.Run(ctx)
			firstResult <- resiliency.ResultWithError[int]{V: result, Err: runErr}
		}()
		select {
		case <-firstStarted:
		case <-ctx.Done():
			test.Fatal("first runner did not start")
		}
		go func() {
			result, runErr := deb.Run(ctx)
			secondResult <- resiliency.ResultWithError[int]{V: result, Err: runErr}
		}()
		select {
		case <-secondStarted:
		case <-ctx.Done():
			test.Fatal("second runner did not start independently")
		}
		close(releaseFirst)
		first := <-firstResult
		require.NoError(test, first.Err)
		require.Equal(test, 1, first.V)
		synctest.Wait()
		select {
		case unexpected := <-secondResult:
			test.Fatalf("second run completed before its runner was released: %+v", unexpected)
		default:
		}
		close(releaseSecond)
		second := <-secondResult
		require.NoError(test, second.Err)
		require.Equal(test, 2, second.V)
		require.EqualValues(test, 2, calls.Load())
	})
}
