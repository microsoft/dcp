package concurrency_test

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const defaultSemaphoreTestTimeout = 20 * time.Second

func withJitter(baseDelay time.Duration) time.Duration {
	return baseDelay + time.Duration(math.Round(rand.Float64()*float64(baseDelay)))
}

// Verifies Waiter is not cancelled until Signal() is called.
func TestSemaphoreWaiterNotCancelledBeforeSignal(t *testing.T) {
	t.Parallel()

	sem := concurrency.NewSemaphore()
	wt := sem.Wait()

	select {
	case <-wt.Chan:
		require.Fail(t, "waiter cancelled before Signal() was called")
	default:
		// Expected
	}

	sem.Signal()

	select {
	case <-wt.Chan:
		// Expected for the waiter to be cancelled
	default:
		require.Fail(t, "waiter was not cancelled after Signal() was called")
	}
}

// Verifies that multiple waiters will complete when corresponding number of Signal() calls are made.
// Signal() and Wait() calls are made in separate goroutines, with random delays.
func TestSemaphoreMultipleWaiters(t *testing.T) {
	t.Parallel()

	sem := concurrency.NewSemaphore()
	const baseDelay = 20 * time.Millisecond
	const numCalls = 50

	ctx, cancel := testutil.GetTestContext(t, defaultSemaphoreTestTimeout)
	defer cancel()

	waitersDone := make(chan struct{})

	// Start goroutines that do waiting and signalling
	go func() {
		defer close(waitersDone)
		waiters := make([]*concurrency.Waiter, numCalls)

		for i := range numCalls {
			waiters[i] = sem.Wait()
			time.Sleep(withJitter(baseDelay))
		}
		for i := range numCalls {
			select {
			case <-waiters[i].Chan:
			case <-ctx.Done():
				require.Fail(t, "Not all Waiters cancelled before timeout. Current waiter count: %d", i)
			}
		}
	}()

	go func() {
		for range numCalls {
			sem.Signal()
			time.Sleep(withJitter(baseDelay))
		}
	}()

	<-waitersDone // This will eventually end because the waiting goroutine is checking the test context
}

// Verifies that multiple calls to Signal() followed by calls to Wait()
// will result in Waiters that are immediately cancelled.
func TestSemaphoreSignalThenWait(t *testing.T) {
	t.Parallel()

	sem := concurrency.NewSemaphore()
	const numCalls = 50

	for range numCalls {
		sem.Signal()
	}

	waiters := make([]*concurrency.Waiter, numCalls)
	for i := range numCalls {
		waiters[i] = sem.Wait()
	}

	for i := range numCalls {
		select {
		case <-waiters[i].Chan:
			// Expected: channel closed immediately
		default:
			require.Fail(t, "waiter should be immediately cancelled after previous signal")
		}
	}
}

// Verifies that multiple calls to Wait() followed by multiple calls to Signal()
// will result in Waiters that are cancelled in FIFO order.
func TestSemaphoreWaitThenSignal(t *testing.T) {
	t.Parallel()

	sem := concurrency.NewSemaphore()
	const numCalls = 50

	waiters := make([]*concurrency.Waiter, numCalls)
	for i := range numCalls {
		waiters[i] = sem.Wait()
	}

	for i := range numCalls {
		sem.Signal()

		select {
		case <-waiters[i].Chan:
			// Expected: waiter cancelled
		default:
			require.Fail(t, "waiter should be cancelled after signal, current waieter count: %d", i)
		}
	}
}

// Verifies that a Waiter can be cancelled before or after corresponding call to Signal() is made.
// In both cases, the Waiter should be cancelled.
func TestSemaphoreWaiterCancellation(t *testing.T) {
	t.Parallel()

	sem := concurrency.NewSemaphore()

	w1 := sem.Wait()
	w1.Cancel()

	select {
	case <-w1.Chan:
		// Expected
	default:
		require.Fail(t, "waiter 1 should be cancelled after cancellation")
	}

	// Should not affect w1 because w1 is already cancelled
	sem.Signal()

	select {
	case <-w1.Chan:
		// Expected
	default:
		require.Fail(t, "waiter 1 should stay cancelled once it has been cancelled")
	}

	w2 := sem.Wait()

	select {
	case <-w2.Chan:
		// Expected because the semaphore was signalled
	default:
		require.Fail(t, "waiter 2 should have been completed after Signal() call")
	}

	w2.Cancel()

	select {
	case <-w2.Chan:
		// Expected
	default:
		require.Fail(t, "waiter 2 should stay cancelled once it has been cancelled")
	}
}
