package concurrency

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRingBufferReturnsDataInOrder(t *testing.T) {
	t.Parallel()

	rb := newRingBuffer[int]()
	require.True(t, rb.Empty())

	for i := 0; i < 100; i++ {
		rb.Push(i)
		require.False(t, rb.Empty())
	}

	for i := 0; i < 100; i++ {
		v, found := rb.Peek()
		require.True(t, found)
		require.Equal(t, i, v)
		v, found = rb.Pop()
		require.True(t, found)
		require.Equal(t, i, v)
	}

	require.True(t, rb.Empty())
}

// Fills and drains the buffer with different amount of data, forcing the internal buffer to grow and shrink.
func TestRingBufferResizing(t *testing.T) {
	t.Parallel()

	rb := newRingBuffer[int]()
	require.True(t, rb.Empty())

	fillAndDrain := func(n int) {
		for i := 0; i < n; i++ {
			rb.Push(i)
		}
		for i := 0; i < n; i++ {
			v, found := rb.Pop()
			require.True(t, found)
			require.Equal(t, i, v)
		}
	}

	fillAndDrain(10)
	fillAndDrain(20)
	fillAndDrain(50)
	fillAndDrain(100)
	fillAndDrain(200)
	fillAndDrain(500)
	fillAndDrain(1000)
}
