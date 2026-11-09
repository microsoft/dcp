/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"bytes"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that overflow-limited pipes reject excess unread data without blocking or partial writes,
// retain the overflow error after close, and allow capacity to be reused after reads.
func TestBufferedPipeOverflowLimit(t *testing.T) {
	for _, limit := range []uint{0, 1, 1024} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			reader, writer := usvc_io.NewBufferedPipeWithOverflowLimit(limit)
			defer reader.Close()
			defer writer.Close()
			payload := bytes.Repeat([]byte("x"), int(limit))
			count, writeErr := writer.Write(payload)
			require.NoError(t, writeErr)
			require.Equal(t, len(payload), count)
			if limit > 0 {
				drained := make([]byte, len(payload))
				_, readErr := io.ReadFull(reader, drained)
				require.NoError(t, readErr)
				require.Equal(t, payload, drained)
				refilled, refillErr := writer.Write(payload)
				require.NoError(t, refillErr)
				require.Equal(t, len(payload), refilled)
			}
			overflowed, overflowErr := writer.Write([]byte("x"))
			require.Zero(t, overflowed)
			require.ErrorIs(t, overflowErr, usvc_io.ErrBufferFull)
			require.NoError(t, writer.Close())
			require.NoError(t, reader.Close())
			_, retainedWriteErr := writer.Write([]byte("x"))
			require.ErrorIs(t, retainedWriteErr, usvc_io.ErrBufferFull)
			_, retainedReadErr := reader.Read(make([]byte, 1))
			require.ErrorIs(t, retainedReadErr, usvc_io.ErrBufferFull)
		})
	}
}

// Verifies that overflowing an empty buffered pipe wakes pending readers with an explicit error.
func TestBufferedPipeOverflowWakesReader(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	reader, writer := usvc_io.NewBufferedPipeWithOverflowLimit(1)
	defer reader.Close()
	defer writer.Close()
	readDone := make(chan error, 1)
	go func() {
		_, readErr := reader.Read(make([]byte, 1))
		readDone <- readErr
	}()
	count, writeErr := writer.Write([]byte("too large"))
	require.Zero(t, count)
	require.ErrorIs(t, writeErr, usvc_io.ErrBufferFull)
	select {
	case readErr := <-readDone:
		require.ErrorIs(t, readErr, usvc_io.ErrBufferFull)
	case <-ctx.Done():
		t.Fatal("buffer overflow left the reader blocked")
	}
}

// Verifies that closing the reader rejects writes in both unlimited and overflow-limited pipes.
func TestBufferedPipeReaderClosureRejectsWrites(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		reader, writer := usvc_io.NewBufferedPipe()
		if overflow {
			require.NoError(t, reader.Close())
			require.NoError(t, writer.Close())
			reader, writer = usvc_io.NewBufferedPipeWithOverflowLimit(16)
		}
		require.NoError(t, reader.Close())
		count, writeErr := writer.Write([]byte("closed"))
		require.Zero(t, count)
		require.ErrorIs(t, writeErr, io.ErrClosedPipe)
		require.NoError(t, writer.Close())
	}
}

func TestMultipleRwOps(t *testing.T) {
	reader, writer := usvc_io.NewBufferedPipe()
	sync := make(chan struct{})

	doReading := func(what string) {
		buf := make([]byte, 100)
		n, err := reader.Read(buf)
		require.NoError(t, err)
		require.Equal(t, len(what), n)
		require.Equal(t, what, string(buf[0:n]))
		sync <- struct{}{}
	}

	doWriting := func(what string) {
		n, err := writer.Write([]byte(what))
		require.NoError(t, err)
		require.Equal(t, len(what), n)
	}

	doWriting("alpha")
	go doReading("alpha")
	<-sync

	doWriting("bravo")
	go doReading("bravo")
	<-sync
}

func TestBufferedPipeMaxSize(t *testing.T) {
	const maxSize uint = 1024 // 1KB max buffer
	reader, writer := usvc_io.NewBufferedPipeWithMaxSize(maxSize)

	var totalWritten atomic.Int64
	writeCompleted := make(chan struct{})

	// Write in a goroutine - should block when buffer is full
	go func() {
		chunk := make([]byte, 256) // 256 byte chunks
		// Try to write 4KB which is well beyond the 1KB limit
		for i := 0; i < 16; i++ {
			n, writeErr := writer.Write(chunk)
			if writeErr != nil {
				break
			}
			totalWritten.Add(int64(n))
		}
		close(writeCompleted)
	}()

	// Wait for writes to fill the buffer and block
	select {
	case <-writeCompleted:
		t.Fatal("Write should have blocked when buffer is full")
	case <-time.After(500 * time.Millisecond):
		// Write blocked as expected
		written := totalWritten.Load()
		require.GreaterOrEqual(t, written, int64(maxSize),
			"Should have written at least maxSize before blocking")
		require.LessOrEqual(t, written, int64(maxSize+256),
			"Should not have written more than maxSize + one chunk")
	}

	// Now read some data to unblock the writer
	buf := make([]byte, 512)
	n, readErr := reader.Read(buf)
	require.NoError(t, readErr)
	require.Greater(t, n, 0)

	// Writer should be able to continue now
	select {
	case <-writeCompleted:
		// Writer completed after we read data
	case <-time.After(500 * time.Millisecond):
		// Writer should have made progress - check it wrote more
		written := totalWritten.Load()
		require.Greater(t, written, int64(maxSize+256),
			"Writer should have made progress after data was consumed")
	}

	// Clean up
	require.NoError(t, writer.Close())
	require.NoError(t, reader.Close())
}

func TestBufferedPipeMaxSizeWriterBlocksUntilRead(t *testing.T) {
	const maxSize uint = 512
	reader, writer := usvc_io.NewBufferedPipeWithMaxSize(maxSize)
	defer reader.Close()
	defer writer.Close()

	// Fill the buffer completely
	data := make([]byte, int(maxSize))
	n, writeErr := writer.Write(data)
	require.NoError(t, writeErr)
	require.Equal(t, int(maxSize), n)

	// Try to write more - should block
	writeBlocked := make(chan struct{})
	go func() {
		_, _ = writer.Write([]byte("extra"))
		close(writeBlocked)
	}()

	// Verify write is blocked
	select {
	case <-writeBlocked:
		t.Fatal("Write should be blocked when buffer is full")
	case <-time.After(200 * time.Millisecond):
		// Expected - write is blocked
	}

	// Read some data to unblock
	buf := make([]byte, 100)
	n, readErr := reader.Read(buf)
	require.NoError(t, readErr)
	require.Greater(t, n, 0)

	// Now the write should complete
	select {
	case <-writeBlocked:
		// Write completed after read
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Write should have unblocked after reading data")
	}
}

func TestBufferedPipeUnlimitedWhenMaxSizeZero(t *testing.T) {
	// maxSize of 0 means unlimited
	reader, writer := usvc_io.NewBufferedPipeWithMaxSize(0)
	defer reader.Close()
	defer writer.Close()

	// Write a large amount of data - should not block
	data := make([]byte, 1024*1024) // 1MB
	writeCompleted := make(chan struct{})

	go func() {
		_, _ = writer.Write(data)
		close(writeCompleted)
	}()

	// Write should complete immediately since there's no limit
	select {
	case <-writeCompleted:
		// Expected - no blocking
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Write should not block when maxSize is 0 (unlimited)")
	}
}
