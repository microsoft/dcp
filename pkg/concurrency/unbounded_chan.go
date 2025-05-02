package concurrency

import (
	"context"
	"sync/atomic"

	"github.com/microsoft/usvc-apiserver/pkg/container"
)

// UnboundedChan implements an unbounded channel that exhibits at most a short block time when writing
// (data will be buffered for unlimited time if it is not read immediately).
// The implementation uses two channels (In and Out) and a buffer, with a dedicated goroutine
// that moves data between the In channel and the buffer, and between the buffer and the Out channel.
// The goroutine exits when the context associated with the UnboundedChan is canceled.
//
// Multiple writers and readers are supported (UnboundedChan is goroutine-safe).
//
// The input channel (In channel) can be closed by UnboundedChan users to signal that no more data will be written.
// If the channel context is not cancelled, the goroutine will continue to write buffered data to the Out channel
// until the buffer is empty. To guarantee that all data written to the input channel makes it to the output channel,
// make sure to use a non-buffered input channel.
//
// Inspired by https://github.com/smallnest/chanx
type UnboundedChan[T any] struct {
	In     chan<- T // channel for writing data
	Out    <-chan T // channel for reading data
	buf    *container.RingBuffer[T]
	bufLen *atomic.Int64
}

// NewUnboundedChan creates a new instance of unbounded channel with unbuffered In and Out channels.
func NewUnboundedChan[T any](ctx context.Context) *UnboundedChan[T] {
	return NewUnboundedChanBuffered[T](ctx, 0, 0)
}

// NewUnboundedChanBuffered creates a new instance of unbounded channel with specified buffer sizes
// for In and Out channels. If either size is zero, the corresponding channel will be unbuffered.
func NewUnboundedChanBuffered[T any](ctx context.Context, inSize, outSize int) *UnboundedChan[T] {
	in := make(chan T, inSize)
	out := make(chan T, outSize)

	ch := UnboundedChan[T]{
		In:     in,
		Out:    out,
		buf:    container.NewRingBuffer[T](),
		bufLen: &atomic.Int64{},
	}

	go ch.process(ctx, in, out)

	return &ch
}

// Returns the number of elements in the buffer.
// This method is safe for concurrent use, but it cannot be relied upon to provide an exact count.
func (ch *UnboundedChan[T]) BufLen() int64 {
	return ch.bufLen.Load()
}

func (ch *UnboundedChan[T]) process(ctx context.Context, in, out chan T) {
	defer close(out)

	drain := func() {
		for {
			v, found := ch.buf.Pop()
			if !found {
				break
			}
			ch.bufLen.Add(-1)

			select {
			case out <- v:
			case <-ctx.Done():
				break
			}
		}
	}
	defer drain()

	for {
		select {

		case <-ctx.Done():
			return

		case val, isOpen := <-in:
			if !isOpen {
				// No more data coming and the buffer is empty.
				return
			}

			select {
			case out <- val:
				// Wrote to out without blocking
				continue // outer "for" loop
			default:
				// Could not write to out without blocking
				ch.buf.Push(val)
				ch.bufLen.Add(1)
			}

			for !ch.buf.Empty() {
				buffered, _ := ch.buf.Peek()

				select {

				case <-ctx.Done():
					return

				case val, isOpen = <-in:
					if !isOpen {
						// No more data coming. Continue the inner loop to drain the buffer,
						// but do not read from the (closed) input channel.
						in = nil
					} else {
						ch.buf.Push(val)
						ch.bufLen.Add(1)
					}

				case out <- buffered:
					// Wrote the buffered value, remove it from the buffer
					_, _ = ch.buf.Pop()
					ch.bufLen.Add(-1)
				}
			}
		}

		if in == nil {
			// It is important that we return here.
			// Otherwise we would block in the outer loop until the context is cancelled.
			return
		}
	}
}
