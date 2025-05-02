package container

const (
	minSize      = 8 // Must be a power of 2
	growthFactor = 2
	shrinkFactor = 4

	UnlimitedCapacity = 0
)

// RingBuffer is a ring buffer that grows as needed when new items are added.
// It is not goroutine-safe.
type RingBuffer[T any] struct {
	buf      []T
	len      int // how many items in the buffer
	head     int //
	tail     int // write index
	capacity int // max number of items in the buffer (0 for unlimited)
}

func NewRingBuffer[T any]() *RingBuffer[T] {
	return &RingBuffer[T]{
		buf:      make([]T, minSize),
		capacity: UnlimitedCapacity,
	}
}

func NewBoundedRingBuffer[T any](capacity int) *RingBuffer[T] {
	if capacity <= 0 {
		return NewRingBuffer[T]() // unlimited capacity
	}

	bufSize := minSize
	for bufSize < capacity {
		bufSize *= growthFactor
	}
	return &RingBuffer[T]{
		buf:      make([]T, bufSize),
		capacity: capacity,
	}
}

// Pushes an item into the buffer.
// If the buffer is full, it will grow to accommodate the new item.
// If the buffer is full and bounded, the oldest item will be discarded.
func (rb *RingBuffer[T]) Push(v T) {
	if rb.capacity != UnlimitedCapacity && rb.len == rb.capacity {
		// At capacity, discard the oldest item
		rb.buf[rb.tail] = v
		rb.tail = rb.next(rb.tail)
		rb.head = rb.next(rb.head)
		return
	}

	if rb.len == len(rb.buf) {
		rb.resize()
	}

	rb.buf[rb.tail] = v
	rb.tail = rb.next(rb.tail)
	rb.len++
}

// Removes and returns the least recently used item from the buffer.
// The second value indicates whether the buffer was empty and a zero-value item was returned instead.
func (rb *RingBuffer[T]) Pop() (T, bool) {
	var zero T
	if rb.len == 0 {
		return zero, false
	}

	v := rb.buf[rb.head]
	rb.buf[rb.head] = zero
	rb.head = rb.next(rb.head)
	rb.len--

	bufSize := len(rb.buf)
	if rb.len <= bufSize/shrinkFactor && rb.len*growthFactor >= minSize {
		rb.resize()
	}

	return v, true
}

// Removes and returns the most recently used item from the buffer.
// The second value indicates whether the buffer was empty and a zero-value item was returned.
func (rb *RingBuffer[T]) PopTail() (T, bool) {
	var zero T
	if rb.len == 0 {
		return zero, false
	}

	rb.tail = rb.prev(rb.tail)
	v := rb.buf[rb.tail]
	rb.buf[rb.tail] = zero
	rb.len--
	return v, true
}

func (rb *RingBuffer[T]) Peek() (T, bool) {
	var zero T
	if rb.len == 0 {
		return zero, false
	}
	return rb.buf[rb.head], true
}

func (rb *RingBuffer[T]) PeekTail() (T, bool) {
	var zero T
	if rb.len == 0 {
		return zero, false
	}
	last := rb.prev(rb.tail)
	return rb.buf[last], true
}

func (rb *RingBuffer[T]) PeekAt(index int) (T, bool) {
	var zero T
	if index < 0 || index >= rb.len {
		return zero, false
	}
	return rb.buf[(rb.head+index)%len(rb.buf)], true
}

func (rb *RingBuffer[T]) Len() int {
	return rb.len
}

func (rb *RingBuffer[T]) Empty() bool {
	return rb.len == 0
}

func (rb *RingBuffer[T]) Capacity() int {
	return rb.capacity
}

func (rb *RingBuffer[T]) next(i int) int {
	return (i + 1) % len(rb.buf)
}

func (rb *RingBuffer[T]) prev(i int) int {
	bufLen := len(rb.buf)
	return (i - 1 + bufLen) % bufLen
}

func (rb *RingBuffer[T]) resize() {
	newSize := rb.len * growthFactor
	newBuf := make([]T, newSize)
	if rb.tail > rb.head {
		copy(newBuf, rb.buf[rb.head:rb.tail])
	} else {
		n := copy(newBuf, rb.buf[rb.head:])
		copy(newBuf[n:], rb.buf[:rb.tail])
	}
	rb.head = 0
	rb.tail = rb.len
	rb.buf = newBuf
}
