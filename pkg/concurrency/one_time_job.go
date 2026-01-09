/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

import (
	"sync"
)

type oneTimeJobState uint8

const (
	oneTimeJobStateInitial oneTimeJobState = iota
	oneTimeJobStateRunning
	oneTimeJobStateDone
)

// OneTimeJob represents an activity that must be done only once.
// Multiple goroutines can try to get the job, but only one of them will succeed.
// Those that fail will wait until the job is done.
type OneTimeJob[T any] struct {
	lock   *sync.Mutex
	done   chan struct{}
	state  oneTimeJobState
	result T
}

func NewOneTimeJob[T any]() *OneTimeJob[T] {
	return &OneTimeJob[T]{
		lock:  &sync.Mutex{},
		done:  make(chan struct{}),
		state: oneTimeJobStateInitial,
	}
}

// Atomically tries to take the job.
// Returns true if the caller "got" the job and is supposed to perform it, otherwise false
// (the job is already taken).
func (otj *OneTimeJob[T]) TryTake() bool {
	otj.lock.Lock()
	defer otj.lock.Unlock()

	if otj.state == oneTimeJobStateInitial {
		// The caller got the job.
		otj.state = oneTimeJobStateRunning
		return true
	}

	return false
}

// Sets the job result and marks the job as done.
func (otj *OneTimeJob[T]) Complete(res T) {
	otj.lock.Lock()
	defer otj.lock.Unlock()

	if otj.state == oneTimeJobStateInitial {
		panic("cannot mark OneTimeJob as done before it is started")
	}

	if otj.state == oneTimeJobStateDone {
		panic("OneTimeJob marked as done more than once")
	}

	otj.state = oneTimeJobStateDone
	otj.result = res
	close(otj.done)
}

// Returns the channel that will be closed when the job is done.
func (otj *OneTimeJob[T]) Done() <-chan struct{} {
	return otj.done
}

// Waits for the job to be done and returns the result.
func (otj *OneTimeJob[T]) WaitResult() T {
	<-otj.done // Channel read establishes happens-before relationship for result read.
	return otj.result
}

// Returns true if the job is done, otherwise false.
func (otj *OneTimeJob[T]) IsDone() bool {
	otj.lock.Lock()
	defer otj.lock.Unlock()

	return otj.state == oneTimeJobStateDone
}
