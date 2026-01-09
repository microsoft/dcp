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
	deb := NewDebounceLastAction(func(_ int) { close(done) }, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	start := time.Now()
	deb.Run(ctx, 0)
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

	deb := NewDebounceLastAction(func(c *atomic.Int32) {
		c.Add(1)
		lastFinished.Store(time.Now())
	}, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	start := time.Now()
	// Make numCalls calls to runner, in order, as fast as possible
	for i := 0; i < numCalls; i++ {
		deb.Run(ctx, &counter)
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

	deb := NewDebounceLastAction(func(e *concurrency.AutoResetEvent) { e.Set() }, debounceDelay, testTimeoutDelay)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeoutDelay)
	defer cancel()

	makeCalls := func() {
		event.Clear()

		for i := 0; i < numCalls; i++ {
			deb.Run(ctx, event)
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
	deb := NewDebounceLastAction(func(b *atomic.Bool) { b.Store(true) }, debounceDelay, time.Second)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeoutDelay)
	defer cancel()
	deb.Run(ctx, &called)

	<-ctx.Done()
	finish := time.Now()

	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	// Assuming debounceDelay is significantly larger than contextTimeoutDelay, the call should return
	// before debounceDelay.
	require.WithinRange(t, finish, start.Add(contextTimeoutDelay), start.Add(debounceDelay))
}
