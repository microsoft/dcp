// Copyright (c) Microsoft Corporation. All rights reserved.

// Package queue implements a generic, thread-safe, bounded queue using single lock and a circular buffer, dynamically sized.
// CONSIDER: a lock-free implementation might perform better.
package queue

import (
	"sync"

	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/container"
)

type ConcurrentBoundedQueue[T any] struct {
	lock    *sync.Mutex
	newData *concurrency.AutoResetEvent
	buf     *container.RingBuffer[T]
}

func NewConcurrentBoundedQueue[T any](capacity int) *ConcurrentBoundedQueue[T] {
	effectiveCapacity := capacity
	if effectiveCapacity <= 0 {
		effectiveCapacity = 1
	}
	return &ConcurrentBoundedQueue[T]{
		lock:    &sync.Mutex{},
		buf:     container.NewBoundedRingBuffer[T](effectiveCapacity),
		newData: concurrency.NewAutoResetEvent(false),
	}
}

func (q *ConcurrentBoundedQueue[T]) Enqueue(v T) {
	q.lock.Lock()
	defer q.lock.Unlock()
	defer q.newData.Set()
	q.buf.Push(v)
}

func (q *ConcurrentBoundedQueue[T]) Dequeue() (T, bool) {
	q.lock.Lock()
	defer q.lock.Unlock()
	return q.buf.Pop()
}

// NewData returns a channel that exposes a new value whenever the queue goes from empty to having some data.
// ONLY ONE CONSUMING GOROUTINE should use this channel.
func (q *ConcurrentBoundedQueue[T]) NewData() <-chan struct{} {
	return q.newData.Wait()
}
