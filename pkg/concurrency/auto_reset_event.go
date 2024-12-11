// Copyright (c) Microsoft Corporation. All rights reserved.

package concurrency

import (
	"sync"
	"sync/atomic"
)

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
