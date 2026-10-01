/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that every accepted registration contributes one completion that
// must be reported before the latch can finish waiting.
func TestCountdownLatchTracksAcceptedCompletions(t *testing.T) {
	t.Parallel()

	latch := concurrency.NewCountdownLatch()
	const registrationCount = 64
	completions := make([]func(), 0, registrationCount)
	for range registrationCount {
		complete, accepted := latch.Register()
		require.True(t, accepted)
		require.NotNil(t, complete)
		completions = append(completions, complete)
	}

	latch.Close()
	var completed atomic.Int32
	var completionCalls sync.WaitGroup
	completionCalls.Add(registrationCount)
	for _, complete := range completions {
		go func() {
			defer completionCalls.Done()
			complete()
			completed.Add(1)
		}()
	}
	latch.Wait()
	completionCalls.Wait()
	require.Equal(t, int32(registrationCount), completed.Load())
}

// Verifies that repeated Close calls are harmless and that the first call
// rejects new registrations without losing previously accepted completions.
func TestCountdownLatchCloseIsIdempotentAndRejectsRegistration(t *testing.T) {
	t.Parallel()

	latch := concurrency.NewCountdownLatch()
	complete, accepted := latch.Register()
	require.True(t, accepted)
	latch.Close()
	latch.Close()
	latch.Close()
	rejectedCompletion, acceptedAfterClose := latch.Register()
	require.False(t, acceptedAfterClose)
	require.Nil(t, rejectedCompletion)
	complete()
	latch.Wait()
}

// Verifies that Wait called after Close remains blocked until all previously
// accepted registrations report completion.
func TestCountdownLatchWaitAfterCloseBlocksForOutstandingCompletions(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 10*time.Second)
	defer testCancel()
	latch := concurrency.NewCountdownLatch()
	firstCompletion, firstAccepted := latch.Register()
	require.True(t, firstAccepted)
	secondCompletion, secondAccepted := latch.Register()
	require.True(t, secondAccepted)
	latch.Close()
	waitStarted := make(chan struct{})
	waitDone := make(chan struct{})
	go func() {
		close(waitStarted)
		latch.Wait()
		close(waitDone)
	}()
	select {
	case <-waitStarted:
	case <-testCtx.Done():
		t.Fatal("Wait did not start")
	}

	select {
	case <-waitDone:
		t.Fatal("Wait returned before any completion")
	default:
	}
	firstCompletion()
	select {
	case <-waitDone:
		t.Fatal("Wait returned before every completion")
	default:
	}
	secondCompletion()
	select {
	case <-waitDone:
	case <-testCtx.Done():
		t.Fatal("Wait did not return after every completion")
	}
}

// Verifies that Wait may start before Close and remains blocked until closing
// begins, even when there are no accepted registrations.
func TestCountdownLatchWaitBeforeCloseBlocksUntilClose(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 10*time.Second)
	defer testCancel()
	latch := concurrency.NewCountdownLatch()
	waitStarted := make(chan struct{})
	waitDone := make(chan struct{})
	go func() {
		close(waitStarted)
		latch.Wait()
		close(waitDone)
	}()
	select {
	case <-waitStarted:
	case <-testCtx.Done():
		t.Fatal("Wait did not start")
	}
	select {
	case <-waitDone:
		t.Fatal("Wait returned before Close")
	default:
	}

	latch.Close()
	select {
	case <-waitDone:
	case <-testCtx.Done():
		t.Fatal("Wait did not return after Close")
	}
}

// Verifies that multiple Wait callers started before and after Close all observe
// the same outstanding completion and return safely once it is reported.
func TestCountdownLatchSupportsMultipleWaiters(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 10*time.Second)
	defer testCancel()
	latch := concurrency.NewCountdownLatch()
	complete, accepted := latch.Register()
	require.True(t, accepted)
	const (
		waitersBeforeClose = 8
		waitersAfterClose  = 8
		totalWaiters       = waitersBeforeClose + waitersAfterClose
	)
	waitStarted := make(chan struct{}, totalWaiters)
	waitDone := make(chan struct{}, totalWaiters)
	startWaiter := func() {
		go func() {
			waitStarted <- struct{}{}
			latch.Wait()
			waitDone <- struct{}{}
		}()
	}
	for range waitersBeforeClose {
		startWaiter()
	}
	for range waitersBeforeClose {
		select {
		case <-waitStarted:
		case <-testCtx.Done():
			t.Fatal("pre-Close waiter did not start")
		}
	}

	latch.Close()
	for range waitersAfterClose {
		startWaiter()
	}
	for range waitersAfterClose {
		select {
		case <-waitStarted:
		case <-testCtx.Done():
			t.Fatal("post-Close waiter did not start")
		}
	}
	select {
	case <-waitDone:
		t.Fatal("a waiter returned before the accepted completion")
	default:
	}

	complete()
	for range totalWaiters {
		select {
		case <-waitDone:
		case <-testCtx.Done():
			t.Fatal("not all waiters returned after completion")
		}
	}
}

// Verifies that registrations racing with Close are either rejected or counted,
// and that waiting after Close observes every accepted completion safely.
func TestCountdownLatchRegistrationCloseRace(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 10*time.Second)
	defer testCancel()
	const (
		iterationCount = 100
		registrarCount = 32
	)
	for range iterationCount {
		latch := concurrency.NewCountdownLatch()
		start := make(chan struct{})
		var acceptedTasks atomic.Int32
		var executedTasks atomic.Int32
		var registrars sync.WaitGroup
		registrars.Add(registrarCount)
		for range registrarCount {
			go func() {
				defer registrars.Done()
				<-start
				complete, accepted := latch.Register()
				if !accepted {
					return
				}
				acceptedTasks.Add(1)
				complete()
				executedTasks.Add(1)
			}()
		}

		closeDone := make(chan struct{})
		go func() {
			<-start
			latch.Close()
			close(closeDone)
		}()
		close(start)
		registrars.Wait()
		select {
		case <-closeDone:
		case <-testCtx.Done():
			t.Fatal("registration/close race did not finish")
		}

		latch.Wait()
		require.Equal(t, acceptedTasks.Load(), executedTasks.Load())
		_, acceptedAfterClose := latch.Register()
		require.False(t, acceptedAfterClose)
	}
}

// Verifies that Start converts context cancellation into closing and does not
// return until every accepted registration reports completion.
func TestCountdownLatchStartClosesAndWaitsForCompletions(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := testutil.GetTestContext(t, 10*time.Second)
	defer testCancel()
	lifetimeCtx, cancelLifetime := context.WithCancel(testCtx)
	defer cancelLifetime()
	latch := concurrency.NewCountdownLatch()
	complete, accepted := latch.Register()
	require.True(t, accepted)
	completed := false
	defer func() {
		if !completed {
			complete()
		}
	}()

	startDone := make(chan error, 1)
	go func() {
		startDone <- latch.Start(lifetimeCtx)
	}()
	cancelLifetime()
	for {
		candidate, candidateAccepted := latch.Register()
		if !candidateAccepted {
			break
		}
		candidate()
		runtime.Gosched()
		select {
		case <-testCtx.Done():
			t.Fatal("Start did not close the latch after cancellation")
		default:
		}
	}
	select {
	case <-startDone:
		t.Fatal("Start returned before the accepted completion")
	default:
	}

	complete()
	completed = true
	select {
	case startErr := <-startDone:
		require.NoError(t, startErr)
	case <-testCtx.Done():
		t.Fatal("Start did not return after the accepted completion")
	}
}
