/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

import (
	"sync"
	"sync/atomic"
)

// The AutoResetEvent is a synchronization primitive that is automatically reset when successfully consumed.
// It supports two use cases:
//
//  1. Multiple-use, multiple-setters, single-consumer.
//     In this case the event is Set() by one or more goroutines and Wait() is called by a single goroutine.
//     The channel returned by Wait() blocks until Set() is called.
//     When the channel read operation returns, the event is "cleared" (reset).
//     The call to Set() may also precede the Wait()/channel read operation.
//     If the event is already set, the channel read operation will return immediately.
//
//     The code that uses the event must ensure that for every Set() call there is AT MOST one attempt
//     to wait on the event channel. The following sequence illustrates the problem:
//
//     goroutine 1: Set()
//     goroutine 1: Set() // The event is already set, so this is a no-op
//     goroutine 2: ch := Wait(); <-ch // Channel read immediately because the event was set. The event is cleared now.
//     goroutine 2: <-ch // Expecting that since Set() was called twice, this will not block.
//     // In fact, this will block until Set() is called again.
//
//     A rule of thumb is that if there is a single event consumer that only cares about THE FINAL STATE protected by the event
//     (and not about intermediate states), then this is the right use case for AutoResetEvent.
//
//  2. Single-use, multiple-setters, multiple-consumer.
//     In this case the event is by calling SetAndFreeze() by one goroutine and Wait() is called
//     by one or more goroutines (consumers). These calls can happen in any order,
//     and it is OK to call SetAndFreeze() multiple times, from multiple goroutines.
//     Once event is frozen, it remains frozen forever, and the read operation on the channel returned by Wait()
//     will always immediately return.
type AutoResetEvent struct {
	channel   chan struct{}
	closeOnce func()
	frozen    *atomic.Bool
}

func NewAutoResetEvent(initialState bool) *AutoResetEvent {
	retval := &AutoResetEvent{
		channel: make(chan struct{}, 1),
		frozen:  &atomic.Bool{},
	}
	retval.closeOnce = sync.OnceFunc(func() {
		retval.frozen.Store(true)
		close(retval.channel)
	})
	if initialState {
		retval.Set()
	}
	return retval
}

func (e *AutoResetEvent) Wait() <-chan struct{} {
	return e.channel
}

func (e *AutoResetEvent) Set() {
	// Non-blocking for caller
	select {
	case e.channel <- struct{}{}:
		// Note: the above will panic if channel is closed; the presence of default clause does not prevent this.
	default:
	}
}

func (e *AutoResetEvent) Clear() {
	// Non-blocking for caller
	select {
	case _, isOpen := <-e.channel:
		if !isOpen {
			panic("Clear() called on frozen event")
		}
	default:
	}
}

func (e *AutoResetEvent) SetAndFreeze() {
	// Makes WaitChannel() return zero value always, effectively making the event set forever.
	// Calls to Set() and Clear() will panic.
	e.closeOnce()
}

func (e *AutoResetEvent) Frozen() bool {
	return e.frozen.Load()
}
