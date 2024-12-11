package concurrency

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const (
	defaultUnboundedChanTestTimeout = 10 * time.Second
)

// Ensures that unbounded channel can handle groups of N reads followed by groups of N writes without blocking.
func TestUnboundedChanReadsAndWrites(t *testing.T) {
	t.Parallel()

	type testCase struct {
		description       string
		ioGroupSize       int
		channelBufferSize int
	}

	testCases := []testCase{
		{"one-nobuffer", 1, 0},
		{"one-one", 1, 1},
		{"one-five", 1, 5},
		{"two-nobuffer", 2, 0},
		{"two-one", 2, 1},
		{"two-five", 2, 5},
		{"ten-nobuffer", 10, 0},
		{"ten-one", 10, 1},
		{"ten-five", 10, 5},
	}

	const tries = 10

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultUnboundedChanTestTimeout)
			defer cancel()

			inputChannelBufferSize := tc.channelBufferSize
			ch := NewUnboundedChanBuffered[int](ctx, inputChannelBufferSize, tc.channelBufferSize)

			for try := 0; try < tries; try++ {
				done := make(chan struct{})

				go func() {
					for i := 0; i < tc.ioGroupSize; i++ {
						select {
						case ch.In <- i:
						case <-ctx.Done():
							require.Fail(t, "timeout writing to channel")
						}
					}
					close(done)
				}()

				select {
				case <-done:
				case <-ctx.Done():
					require.Fail(t, "timeout waiting for all writes to complete")
				}

				for i := 0; i < tc.ioGroupSize; i++ {
					select {
					case v := <-ch.Out:
						require.Equalf(t, i, v, "data should be returned in order: expected %d, got %d", i, v)
					case <-ctx.Done():
						require.Fail(t, "timeout reading from channel")
					}
				}
			}
		})
	}
}

// Ensures that several goroutines can write and read from the channel concurrently without losing data
func TestUnboundedChanRwImmediateCancellation(t *testing.T) {
	t.Parallel()

	type testCase struct {
		description       string
		channelBufferSize int
	}

	testCases := []testCase{
		{"nobuffer", 0},
		{"one", 1},
		{"five", 5},
	}

	const goroutines = 5
	const writes = 10000

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			ctx, cancel := testutil.GetTestContext(t, defaultUnboundedChanTestTimeout)
			defer cancel()

			ch := NewUnboundedChanBuffered[int](ctx, tc.channelBufferSize, tc.channelBufferSize)
			result := syncmap.Map[int, int]{}

			// Writing goroutines
			for i := 0; i < goroutines; i++ {
				go func(rt int) {
					offset := rt * writes
					for j := 0; j < writes; j++ {
						select {
						case ch.In <- j + offset:
						case <-ctx.Done():
							require.Fail(t, "timeout writing to channel")
						}
					}
				}(i)
			}

			// Reading goroutines
			for i := 0; i < goroutines; i++ {
				go func(rt int) {
					for {
						v, isOpen := <-ch.Out
						if !isOpen {
							return
						}
						_, found := result.LoadOrStore(v, 0)
						require.Falsef(t, found, "duplicate value %d", v)
					}
				}(i)
			}

			waitErr := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, false /* poll immediately */, func(ctx context.Context) (bool, error) {
				count := 0
				result.Range(func(_, _ int) bool {
					count++
					return true
				})
				return count == goroutines*writes, nil
			})
			require.NoErrorf(t, waitErr, "timeout waiting for all expected data to be read")
		})
	}
}

// Ensures that several goroutines can write and read from the channel without losing data
// The channel uses a combination of unbuffered input channel and closing the input channel without cancelling the context
// to guarantee that data written to the UnboundedChan will all be read from the output channel.
func TestUnboundedChanRwDrainCancellation(t *testing.T) {
	t.Parallel()

	type testCase struct {
		description       string
		channelBufferSize int
	}

	testCases := []testCase{
		{"nobuffer", 0},
		{"one", 1},
		{"five", 5},
	}

	const goroutines = 5
	const writes = 10000

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			testCtx, testCtxCancel := testutil.GetTestContext(t, defaultUnboundedChanTestTimeout)
			defer testCtxCancel()

			channelCtx, channelCtxCancel := context.WithCancel(testCtx)
			defer channelCtxCancel()

			ch := NewUnboundedChanBuffered[int](channelCtx, 0, tc.channelBufferSize)
			result := syncmap.Map[int, int]{}
			writingDone := make(chan struct{}, goroutines)

			// Writing goroutines
			for i := 0; i < goroutines; i++ {
				go func(rt int) {
					offset := rt * writes
					for j := 0; j < writes; j++ {
						select {
						case ch.In <- j + offset:
						case <-testCtx.Done():
							require.Fail(t, "timeout writing to channel")
						}
					}
					writingDone <- struct{}{}
				}(i)
			}

			// Reading goroutines
			for i := 0; i < goroutines; i++ {
				go func(rt int) {
					for {
						v, isOpen := <-ch.Out
						if !isOpen {
							return
						}
						_, found := result.LoadOrStore(v, 0)
						require.Falsef(t, found, "duplicate value %d", v)
					}
				}(i)
			}

			// Wait for all writing goroutines to complete
			for i := 0; i < goroutines; i++ {
				select {
				case <-writingDone:
				case <-testCtx.Done():
					require.Fail(t, "timeout waiting for all writes to complete")
				}
			}

			// All the writing goroutines have completed, tell the channel that no more data is coming
			close(ch.In)

			// All expected data should still arrive because the channel should "drain" them into its Out channel

			var count int
			waitErr := wait.PollUntilContextCancel(testCtx, 100*time.Millisecond, true /* poll immediately */, func(ctx context.Context) (bool, error) {
				count = 0
				result.Range(func(_, _ int) bool {
					count++
					return true
				})
				return count == goroutines*writes, nil
			})
			require.NoErrorf(t, waitErr, "timeout waiting for all expected data to be read, only accounted for %d items, expected %d", count, goroutines*writes)
		})
	}
}
