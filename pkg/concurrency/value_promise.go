/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

import (
	"sync"
)

// ValuePromise represents a value that will (might be) be set at some point in the future.
// Clients can wait for the value to be set via a channel.
// The value can be set only once, and subsequent attempts to set are a no-op.
// All methods are safe for concurrent use.
type ValuePromise[T any] struct {
	haveValue    *AutoResetEvent
	valueChan    chan T
	getValueOnce func() T
}

func NewValuePromise[T any]() *ValuePromise[T] {
	result := ValuePromise[T]{
		haveValue: NewAutoResetEvent(false),
		valueChan: make(chan T, 1),
	}
	result.getValueOnce = sync.OnceValue(func() T {
		return <-result.valueChan
	})
	return &result
}

// Sets the value of the promise.
// Returns true if the value was set, false if it was already set.
func (p *ValuePromise[T]) Set(value T) bool {
	select {
	case p.valueChan <- value:
		p.haveValue.SetAndFreeze()
		return true
	default:
		return false
	}
}

// Gets the value of the promise.
// This method will block until the value is set.
func (p *ValuePromise[T]) Get() T {
	return p.getValueOnce()
}

func (p *ValuePromise[T]) IsSet() bool {
	return p.haveValue.Frozen()
}

func (p *ValuePromise[T]) Wait() <-chan struct{} {
	return p.haveValue.Wait()
}
