// Copyright (c) Microsoft Corporation. All rights reserved.

// Package queue implements a generic, thread-safe, bounded queue using single lock and a circular buffer, dynamically sized.
// CONSIDER: a lock-free implementation might perform better.
package queue

import (
	"sync"

	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
)

// Minimum size of the queue buffer. Must be a power of 2.
const (
	minSize      = 8
	growthFactor = 2
	shrinkFactor = 4
)

type ConcurrentBoundedQueue[T any] struct {
	head     int
	tail     int
	lock     *sync.Mutex
	buf      []T
	capacity int
	len      int
	newData  *concurrency.AutoResetEvent
}

func NewConcurrentBoundedQueue[T any](capacity int) *ConcurrentBoundedQueue[T] {
	effectiveCapacity := capacity
	if effectiveCapacity <= 0 {
		effectiveCapacity = 1
	}
	bufSize := minSize
	for bufSize < effectiveCapacity {
		bufSize *= 2
	}
	return &ConcurrentBoundedQueue[T]{
		head:     0,
		tail:     0,
		lock:     &sync.Mutex{},
		capacity: effectiveCapacity,
		buf:      make([]T, bufSize),
		newData:  concurrency.NewAutoResetEvent(false),
	}
}

func (q *ConcurrentBoundedQueue[T]) Enqueue(v T) {
	q.lock.Lock()
	defer q.lock.Unlock()
	defer q.newData.Set()

	if q.len == q.capacity {
		// At capacity, discard the oldest item
		q.buf[q.tail] = v
		q.tail = q.next(q.tail)
		// Note the buf might be larger than q.capacity, so tail is not necessarily equal to head.
		q.head = q.next(q.head)
		return
	}

	if q.len == len(q.buf) {
		// Grow the buffer
		q.resize()
	}

	q.buf[q.tail] = v
	q.tail = q.next(q.tail)
	q.len++
}

func (q *ConcurrentBoundedQueue[T]) Dequeue() (T, bool) {
	var zero T // Zero value
	q.lock.Lock()
	defer q.lock.Unlock()

	if q.len == 0 {
		return zero, false
	}

	v := q.buf[q.head]
	q.buf[q.head] = zero
	q.head = q.next(q.head)
	q.len--

	bufSize := len(q.buf)
	if q.len*growthFactor >= minSize && q.len <= bufSize/shrinkFactor {
		// Shrink the buffer
		q.resize()
	}

	return v, true
}

func (q *ConcurrentBoundedQueue[T]) NewData() <-chan struct{} {
	return q.newData.Wait()
}

func (q *ConcurrentBoundedQueue[T]) next(i int) int {
	return (i + 1) % len(q.buf)
}

func (q *ConcurrentBoundedQueue[T]) resize() {
	newSize := q.len * growthFactor
	newBuf := make([]T, newSize)
	if q.tail > q.head {
		copy(newBuf, q.buf[q.head:q.tail])
	} else {
		n := copy(newBuf, q.buf[q.head:])
		copy(newBuf[n:], q.buf[:q.tail])
	}
	q.head = 0
	q.tail = q.len
	q.buf = newBuf
}
