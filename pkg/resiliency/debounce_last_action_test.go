/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package resiliency

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/concurrency"
)

func TestExecutesActionRunnerAfterDelay(t *testing.T) {
	t.Parallel()

	const debounceDelay = time.Millisecond * 100
	const testTimeoutDelay = time.Millisecond * 1000

	done := make(chan struct{})
	deb := NewDebounceLastAction(func() { close(done) }, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	start := time.Now()
	deb.Run(ctx)
	<-done
	finish := time.Now()

	// The call should happen after debounceDelay
	require.WithinRange(t, finish, start.Add(debounceDelay), time.Now().Add(testTimeoutDelay))
}

func TestDebounceActionRapidInvocations(t *testing.T) {
	t.Parallel()

	const numCalls = 5
	counter := atomic.Int32{}
	lastFinished := &concurrency.AtomicTime{}

	const debounceDelay = time.Millisecond * 200
	const testTimeoutDelay = time.Millisecond * 1000

	deb := NewDebounceLastAction(func() {
		counter.Add(1)
		lastFinished.Store(time.Now())
	}, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	start := time.Now()
	// Make numCalls calls to runner, in order, as fast as possible
	for i := 0; i < numCalls; i++ {
		deb.Run(ctx)
	}

	time.Sleep(testTimeoutDelay)

	require.WithinRange(t, lastFinished.Load(), start.Add(debounceDelay), time.Now().Add(testTimeoutDelay))
}

func TestDebounceActionIsReusable(t *testing.T) {
	t.Parallel()

	const debounceDelay = time.Millisecond * 150
	const testTimeoutDelay = time.Millisecond * 1000

	const numCalls = 3
	event := concurrency.NewAutoResetEvent(false)

	deb := NewDebounceLastAction(func() { event.Set() }, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	makeCalls := func() {
		event.Clear()

		for i := 0; i < numCalls; i++ {
			deb.Run(ctx)
		}

		<-event.Wait()
	}

	start := time.Now()
	testTimeout := start.Add(testTimeoutDelay)
	makeCalls()
	finish := time.Now()

	require.WithinRange(t, finish, start.Add(debounceDelay), testTimeout)

	// Now the same debounce should be ready for another round of calls

	start = time.Now()
	makeCalls()
	finish = time.Now()

	require.WithinRange(t, finish, start.Add(debounceDelay), testTimeout)
}

func TestDebounceActionDoesNotExecuteRunnerIfContextCancelled(t *testing.T) {
	t.Parallel()

	const debounceDelay = time.Millisecond * 500
	const contextTimeoutDelay = time.Millisecond * 100

	called := atomic.Bool{}
	deb := NewDebounceLastAction(func() { called.Store(true) }, debounceDelay, time.Second)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeoutDelay)
	defer cancel()
	deb.Run(ctx)

	<-ctx.Done()
	finish := time.Now()

	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	// Assuming debounceDelay is significantly larger than contextTimeoutDelay, the call should return
	// before debounceDelay.
	require.WithinRange(t, finish, start.Add(contextTimeoutDelay), start.Add(debounceDelay))
}

func TestRunDuringActionExecutionStartsNewRun(t *testing.T) {
	t.Parallel()

	const debounceDelay = time.Millisecond * 20
	const testTimeoutDelay = time.Second * 2

	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	firstFinished := make(chan struct{})
	secondFinished := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	actionCalls := atomic.Int32{}

	deb := NewDebounceLastAction(func() {
		call := actionCalls.Add(1)
		switch call {
		case 1:
			close(firstStarted)
			<-releaseFirst
			close(firstFinished)
		case 2:
			close(secondStarted)
			<-releaseSecond
			close(secondFinished)
		}
	}, debounceDelay, testTimeoutDelay)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	deb.Run(ctx)

	select {
	case <-firstStarted:
	case <-time.After(testTimeoutDelay):
		require.FailNow(t, "first action did not start in time")
	}

	deb.Run(ctx)

	select {
	case <-secondStarted:
	case <-time.After(testTimeoutDelay):
		require.FailNow(t, "second action did not start as a new run")
	}

	close(releaseFirst)
	<-firstFinished

	close(releaseSecond)
	<-secondFinished

	require.Equal(t, int32(2), actionCalls.Load())
}
