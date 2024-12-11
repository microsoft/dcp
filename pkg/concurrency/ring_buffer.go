package concurrency

const (
	minSize      = 8 // Must be a power of 2
	growthFactor = 2
	shrinkFactor = 4
)

// ringBuffer is a ring buffer that grows as needed when new items are added.
// It is not goroutine-safe.
type ringBuffer[T any] struct {
	buf  []T
	len  int // how many items in the buffer
	head int
	tail int // write pointer
}

func newRingBuffer[T any]() *ringBuffer[T] {
	return &ringBuffer[T]{
		buf: make([]T, minSize),
	}
}

// Returns the least recently used item from the buffer.
// The second value indicates whether the buffer was empty and a zero-value item was returned instead.
func (rb *ringBuffer[T]) Pop() (T, bool) {
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

func (rb *ringBuffer[T]) Peek() (T, bool) {
	var zero T
	if rb.len == 0 {
		return zero, false
	}
	return rb.buf[rb.head], true
}

func (rb *ringBuffer[T]) Empty() bool {
	return rb.len == 0
}

func (rb *ringBuffer[T]) Push(v T) {
	if rb.len == len(rb.buf) {
		rb.resize()
	}
	rb.buf[rb.tail] = v
	rb.tail = rb.next(rb.tail)
	rb.len++
}

func (rb *ringBuffer[T]) next(i int) int {
	return (i + 1) % len(rb.buf)
}

func (rb *ringBuffer[T]) resize() {
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
