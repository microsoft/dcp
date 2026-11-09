/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency_test

import (
	"context"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/testutil"
)

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
// and that Wait remains blocked until every accepted completion is reported.
func TestCountdownLatchRegistrationCloseRace(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(test *testing.T) {
		const (
			iterationCount = 100
			registrarCount = 32
		)
		type registrationResult struct {
			complete func()
			accepted bool
		}
		for range iterationCount {
			latch := concurrency.NewCountdownLatch()
			start := make(chan struct{})
			firstRegistrationDone := make(chan struct{})
			registrations := make(chan registrationResult, registrarCount)
			for registrarIndex := range registrarCount {
				go func() {
					<-start
					complete, accepted := latch.Register()
					registrations <- registrationResult{complete: complete, accepted: accepted}
					if registrarIndex == 0 {
						close(firstRegistrationDone)
					}
				}()
			}

			closeDone := make(chan struct{})
			go func() {
				// Ensure at least one accepted task while the other registrations race with Close.
				<-firstRegistrationDone
				latch.Close()
				close(closeDone)
			}()
			close(start)

			var completions []func()
			for range registrarCount {
				result := <-registrations
				if result.accepted {
					require.NotNil(test, result.complete)
					completions = append(completions, result.complete)
				} else {
					require.Nil(test, result.complete)
				}
			}
			<-closeDone
			require.NotEmpty(test, completions)
			rejectedCompletion, acceptedAfterClose := latch.Register()
			require.False(test, acceptedAfterClose)
			require.Nil(test, rejectedCompletion)

			waitDone := make(chan struct{})
			go func() {
				latch.Wait()
				close(waitDone)
			}()
			for _, complete := range completions {
				synctest.Wait()
				select {
				case <-waitDone:
					test.Fatal("Wait returned before every accepted completion")
				default:
				}
				complete()
			}
			synctest.Wait()
			select {
			case <-waitDone:
			default:
				test.Fatal("Wait did not return after every accepted completion")
			}
		}
	})
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
