/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/resiliency"
)

// Verifies that DebounceLastAction executes its action once after the configured delay.
func TestExecutesActionRunnerAfterDelay(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		calls := atomic.Int32{}
		done := make(chan struct{})
		deb := resiliency.NewDebounceLastAction(func() { calls.Add(1); close(done) }, time.Second, 5*time.Second)
		started := time.Now()
		deb.Run(ctx)
		synctest.Wait()
		require.Zero(test, calls.Load())
		select {
		case <-done:
		case <-ctx.Done():
			test.Fatal("debounced action did not execute")
		}
		require.EqualValues(test, 1, calls.Load())
		require.Equal(test, time.Second, time.Since(started))
	})
}

// Verifies that DebounceLastAction postpones execution after each rapid call and executes only once.
func TestDebounceActionRapidInvocations(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		calls := atomic.Int32{}
		done := make(chan struct{})
		deb := resiliency.NewDebounceLastAction(func() { calls.Add(1); close(done) }, time.Second, 5*time.Second)
		for index := range 5 {
			if index != 0 {
				time.Sleep(100 * time.Millisecond)
			}
			deb.Run(ctx)
			synctest.Wait()
			require.Zero(test, calls.Load())
		}
		lastCall := time.Now()
		select {
		case <-done:
		case <-ctx.Done():
			test.Fatal("coalesced action did not execute")
		}
		synctest.Wait()
		require.EqualValues(test, 1, calls.Load())
		require.Equal(test, time.Second, time.Since(lastCall))
	})
}

// Verifies that DebounceLastAction starts a fresh coalesced action after each completed round.
func TestDebounceActionIsReusable(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		calls := atomic.Int32{}
		done := make(chan struct{}, 1)
		deb := resiliency.NewDebounceLastAction(func() { calls.Add(1); done <- struct{}{} }, time.Second, 5*time.Second)
		for round := 1; round <= 2; round++ {
			started := time.Now()
			for range 3 {
				deb.Run(ctx)
			}
			synctest.Wait()
			require.EqualValues(test, round-1, calls.Load())
			select {
			case <-done:
			case <-ctx.Done():
				test.Fatal("reused debounce action did not execute")
			}
			synctest.Wait()
			require.EqualValues(test, round, calls.Load())
			require.Equal(test, time.Second, time.Since(started))
		}
	})
}

// Verifies that a new DebounceLastAction run can execute while the previous action is blocked,
// and releasing one action does not release the other.
func TestRunDuringActionExecutionStartsNewRun(t *testing.T) {
	t.Parallel()
	runDebounceTest(t, func(test *testing.T, ctx context.Context) {
		firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
		firstFinished, secondFinished := make(chan struct{}), make(chan struct{})
		releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
		calls := atomic.Int32{}
		deb := resiliency.NewDebounceLastAction(func() {
			var release <-chan struct{}
			switch calls.Add(1) {
			case 1:
				close(firstStarted)
				defer close(firstFinished)
				release = releaseFirst
			case 2:
				close(secondStarted)
				defer close(secondFinished)
				release = releaseSecond
			default:
				test.Error("unexpected action invocation")
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}, time.Second, 5*time.Second)
		deb.Run(ctx)
		select {
		case <-firstStarted:
		case <-ctx.Done():
			test.Fatal("first action did not start")
		}
		deb.Run(ctx)
		select {
		case <-secondStarted:
		case <-ctx.Done():
			test.Fatal("second action did not start independently")
		}
		close(releaseFirst)
		<-firstFinished
		synctest.Wait()
		select {
		case <-secondFinished:
			test.Fatal("second action completed before it was released")
		default:
		}
		close(releaseSecond)
		<-secondFinished
		require.EqualValues(test, 2, calls.Load())
	})
}
