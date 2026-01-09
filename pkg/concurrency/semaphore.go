/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package concurrency

import (
	"sync"

	"github.com/microsoft/dcp/pkg/container"
)

type waiterHandle uint64

const invalidWaiterHandle waiterHandle = 0

// Semaphore implements a counting semaphore synchronization primitive.
// The waiting goroutines are woken up in FIFO order.
type Semaphore struct {
	lock       *sync.Mutex
	count      uint
	nextHandle waiterHandle
	waiters    map[waiterHandle]*Waiter
	wq         *container.RingBuffer[waiterHandle]
}

func NewSemaphore() *Semaphore {
	return NewSemaphoreWithCount(0)
}

func NewSemaphoreWithCount(initialCount uint) *Semaphore {
	sem := &Semaphore{
		lock:       &sync.Mutex{},
		count:      initialCount,
		nextHandle: invalidWaiterHandle + 1,
		waiters:    make(map[waiterHandle]*Waiter),
		wq:         container.NewRingBuffer[waiterHandle](),
	}
	return sem
}

// Signal() increments the semaphore count and notifies a waiting goroutine, if any.
func (s *Semaphore) Signal() {
	s.lock.Lock()
	defer s.lock.Unlock()

	wh, found := s.wq.Pop()
	if found {
		w, ok := s.waiters[wh]
		if !ok {
			panic("waiter not found in waiters map")
		}
		w.handle = invalidWaiterHandle
		delete(s.waiters, wh)
		close(w.Chan)
	} else {
		s.count++
	}
}

// Wait() returns a Waiter that the caller can use to wait for the semaphore to be signaled.
func (s *Semaphore) Wait() *Waiter {
	s.lock.Lock()
	defer s.lock.Unlock()

	var w *Waiter

	if s.count > 0 {
		s.count--

		// Completed Waiter
		w = &Waiter{
			Chan:   make(chan struct{}),
			handle: invalidWaiterHandle,
			sem:    s, // Still want to keep a reference to avoid races via Semaphore lock
		}
		close(w.Chan)
	} else {
		w = &Waiter{
			Chan:   make(chan struct{}),
			handle: s.nextHandle,
			sem:    s,
		}

		s.nextHandle++
		s.waiters[w.handle] = w
		s.wq.Push(w.handle)
	}

	return w
}

func (s *Semaphore) forget(handle waiterHandle) {
	// Assumes the Semaphore lock is already held
	delete(s.waiters, handle)

	newQ := container.NewRingBuffer[waiterHandle]()
	nHandles := s.wq.Len()
	for range nHandles {
		h, _ := s.wq.Pop()
		if h != handle {
			newQ.Push(h)
		}
	}
	s.wq = newQ
}

// Waiter is a type that exposes a channel for waiting on a Semaphore, and a method to "cancel" the wait.
// If the Waiter is cancelled after the wait completed (i.e. the waiter channel is already closed),
// Cancel() is a no-op. Otherwise, the Cancel() method closes the channel and removes the Waiter
// from the Semaphore's waiters list.
type Waiter struct {
	Chan   chan struct{}
	handle waiterHandle
	sem    *Semaphore
}

func (w *Waiter) Cancel() {
	w.sem.lock.Lock()
	defer w.sem.lock.Unlock()

	if w.handle == invalidWaiterHandle {
		return // Already cancelled
	}

	w.sem.forget(w.handle)
	w.handle = invalidWaiterHandle
	close(w.Chan)
}
