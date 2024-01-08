package queue

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQueueReturnsDataInOrder(t *testing.T) {
	t.Parallel()

	q := NewConcurrentBoundedQueue[int](100)
	for i := 0; i < 100; i++ {
		q.Enqueue(i)
	}
	for i := 0; i < 100; i++ {
		v, ok := q.Dequeue()
		require.True(t, ok)
		require.Equal(t, i, v)
	}
}

// Good test to run with -race option
func TestQueueMultipleGoroutines(t *testing.T) {
	t.Parallel()

	q := NewConcurrentBoundedQueue[int](2000)
	wg := sync.WaitGroup{}
	wg.Add(3)
	result := make([]int, 2000)

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			q.Enqueue(i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			q.Enqueue(i + 1000)
		}
	}()
	go func() {
		defer wg.Done()
		i := 0
		for {
			v, ok := q.Dequeue()
			if ok {
				result[i] = v
				i++
				if i == 2000 {
					break
				}
			}
		}
	}()

	wg.Wait()

	slices.Sort(result)
	for i := 0; i < 2000; i++ {
		require.Equal(t, i, result[i])
	}
}

func TestQueueAtCapacity(t *testing.T) {
	t.Parallel()

	q := NewConcurrentBoundedQueue[int](10)
	for i := 0; i < 20; i++ {
		q.Enqueue(i)
	}
	for i := 0; i < 10; i++ {
		v, ok := q.Dequeue()
		require.True(t, ok)
		require.Equal(t, i+10, v)
	}
}

// Another test to run with -race
func TestQueueAtCapacityWithMultipleGoroutines(t *testing.T) {
	t.Parallel()

	q := NewConcurrentBoundedQueue[int](100)
	wg := sync.WaitGroup{}
	wg.Add(2)
	enqueuedTwoHundred := make(chan struct{})
	result := make([]int, 100)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			q.Enqueue(i)
			if i == 200 {
				close(enqueuedTwoHundred)
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-enqueuedTwoHundred
		for {
			v, ok := q.Dequeue()
			if ok {
				result = append(result, v)
				if v == 499 {
					break
				}
			}
		}
	}()

	wg.Wait()

	// No matter how many items were discarded because the queue was at capacity, the last 100 items should be 400...499
	result = result[len(result)-100:]
	for i := 0; i < 100; i++ {
		require.Equal(t, i+400, result[i])
	}
}

// Fills and drains the queue with different amount of data, forcig the internal buffer to grow and shrink.
func TestQueueResizing(t *testing.T) {
	t.Parallel()

	q := NewConcurrentBoundedQueue[int](1000)

	fillAndDrain := func(n int) {
		for i := 0; i < n; i++ {
			q.Enqueue(i)
		}
		for i := 0; i < n; i++ {
			v, ok := q.Dequeue()
			require.True(t, ok)
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

// Similar to TestQueueMultipleGoroutines, but uses the NewDataNotification channel to wait for data to be available instead of just polling.
func TestNewDataNotification(t *testing.T) {
	q := NewConcurrentBoundedQueue[int](1000)
	wg := sync.WaitGroup{}
	wg.Add(3)
	result := make([]int, 1000)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			q.Enqueue(i)
			if i%100 == 0 {
				// Add a bit of randomness to the producer timing.
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			q.Enqueue(i + 500)
			if i%150 == 0 {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	go func() {
		defer wg.Done()
		i := 0
		newDataAvailable := q.NewData()

		for {
			<-newDataAvailable

			for {
				v, ok := q.Dequeue()
				if !ok {
					break
				}
				result[i] = v
				i++
			}

			if i == 1000 {
				return
			}
		}
	}()

	wg.Wait()

	slices.Sort(result)
	for i := 0; i < 1000; i++ {
		require.Equal(t, i, result[i])
	}
}
