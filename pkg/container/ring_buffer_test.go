package container

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRingBufferReturnsDataInOrder(t *testing.T) {
	t.Parallel()

	rb := NewRingBuffer[int]()
	require.True(t, rb.Empty())

	for i := 0; i < 100; i++ {
		rb.Push(i)
		require.False(t, rb.Empty())
		v, found := rb.PeekTail()
		require.True(t, found)
		require.Equal(t, i, v)

		for j := 0; j < i; j++ {
			v, found = rb.PeekAt(j)
			require.True(t, found)
			require.Equal(t, j, v)
		}
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

	rb := NewRingBuffer[int]()
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

func TestRingBufferAtCapacity(t *testing.T) {
	t.Parallel()

	const cap = 10
	rb := NewBoundedRingBuffer[int](cap)
	require.True(t, rb.Empty())

	for i := 0; i < 2*cap; i++ {
		rb.Push(i)
		require.False(t, rb.Empty())
		require.LessOrEqual(t, rb.Len(), cap)
		v, found := rb.PeekTail()
		require.True(t, found)
		require.Equal(t, i, v)
	}

	for i := 0; i < cap; i++ {
		v, found := rb.Peek()
		require.True(t, found)
		require.Equal(t, i+cap, v)
		v, found = rb.Pop()
		require.True(t, found)
		require.Equal(t, i+cap, v)
	}

	require.True(t, rb.Empty())
	require.Equal(t, 0, rb.Len())
}

func TestRingBufferPopTail(t *testing.T) {
	t.Parallel()

	rb := NewRingBuffer[int]()
	require.True(t, rb.Empty())

	// Test empty buffer
	v, found := rb.PopTail()
	require.False(t, found)
	require.Equal(t, 0, v)

	// Push some items
	for i := 0; i < 10; i++ {
		rb.Push(i)
	}
	require.Equal(t, 10, rb.Len())

	// Pop from tail (LIFO order)
	for i := 9; i >= 0; i-- {
		v, found = rb.PopTail()
		require.True(t, found)
		require.Equal(t, i, v)
	}
	require.True(t, rb.Empty())

	// Test with a bounded buffer
	bounded := NewBoundedRingBuffer[int](5)
	for i := 0; i < 10; i++ {
		bounded.Push(i)
	}
	require.Equal(t, 5, bounded.Len())

	// Ensure we get the most recent 5 items
	for i := 9; i >= 5; i-- {
		v, found = bounded.PopTail()
		require.True(t, found)
		require.Equal(t, i, v)
	}
	require.True(t, bounded.Empty())
}
