// Copyright (c) Microsoft Corporation. All rights reserved.

package concurrency

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInitialEventState(t *testing.T) {
	t.Parallel()

	event := NewAutoResetEvent(false)
	ensureEventNotSet(t, event)

	event = NewAutoResetEvent(true)
	ensureEventSet(t, event)
}

func TestSetEvent(t *testing.T) {
	t.Parallel()

	event := NewAutoResetEvent(false)
	ensureEventNotSet(t, event)

	event.Set()
	ensureEventSet(t, event)
	// After the event has been consumed, it should flip back to not set.
	ensureEventNotSet(t, event)
}

func TestClearEvent(t *testing.T) {
	t.Parallel()

	event := NewAutoResetEvent(true)
	event.Clear()
	ensureEventNotSet(t, event)

	event.Set()
	event.Clear()
	ensureEventNotSet(t, event)
}

func TestEventMultipleWaiters(t *testing.T) {
	t.Parallel()

	event := NewAutoResetEvent(false)
	ensureEventNotSet(t, event)

	const numWaiters = 3
	waiterDone := make(chan struct{})

	for i := 0; i < numWaiters; i++ {
		go func() {
			<-event.Wait()
			waiterDone <- struct{}{}
		}()
	}

	for i := 0; i < numWaiters; i++ {
		event.Set()

		// Calling AutoResetEvent.Set() in rapid succession is essentially the same as calling it once.
		// That is why we need to use a channel to ensure that one of the waiters has noticed the event being set.
		<-waiterDone
	}
}

func TestFreezeEvent(t *testing.T) {
	t.Parallel()

	event := NewAutoResetEvent(false)
	ensureEventNotSet(t, event)
	require.False(t, event.Frozen())

	event.SetAndFreeze()
	ensureEventSet(t, event)
	ensureEventSet(t, event)
	require.True(t, event.Frozen())

	// Can call SetAndFreeze() multiple times.
	require.NotPanics(t, event.SetAndFreeze)
	ensureEventSet(t, event)
	require.True(t, event.Frozen())

	require.Panics(t, func() { event.Set() })
	require.Panics(t, func() { event.Clear() })
}

func ensureEventSet(t *testing.T, event *AutoResetEvent) {
	select {
	case <-event.Wait():
	default:
		require.Fail(t, "event not set")
	}
}

func ensureEventNotSet(t *testing.T, event *AutoResetEvent) {
	select {
	case <-event.Wait():
		require.Fail(t, "event set")
	default:
	}
}
