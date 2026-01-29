/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

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
