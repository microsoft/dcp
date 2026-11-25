/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"
	"testing/synctest"

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

// Verifies that a size-limited pipe blocks at capacity and resumes writing after data is consumed.
func TestBufferedPipeMaxSize(t *testing.T) {
	synctest.Test(t, func(test *testing.T) {
		const maxSize uint = 1024
		reader, writer := usvc_io.NewBufferedPipeWithMaxSize(maxSize)
		defer reader.Close()
		defer writer.Close()
		totalWritten := 0
		writeCompleted := make(chan struct{})
		go func() {
			defer close(writeCompleted)
			chunk := make([]byte, 256)
			for range 16 {
				count, writeErr := writer.Write(chunk)
				totalWritten += count
				if writeErr != nil {
					return
				}
			}
		}()
		synctest.Wait()
		select {
		case <-writeCompleted:
			test.Fatal("writer completed despite a full buffer and no reader")
		default:
		}
		require.Equal(test, int(maxSize), totalWritten)

		buffer := make([]byte, 512)
		count, readErr := reader.Read(buffer)
		require.NoError(test, readErr)
		require.Equal(test, len(buffer), count)
		synctest.Wait()
		require.Equal(test, int(maxSize)+len(buffer), totalWritten)
		select {
		case <-writeCompleted:
			test.Fatal("writer completed despite filling the buffer again")
		default:
		}

		require.NoError(test, reader.Close())
		<-writeCompleted
		require.NoError(test, writer.Close())
	})
}

// Verifies that consuming buffered data releases a writer blocked at the configured capacity.
func TestBufferedPipeMaxSizeWriterBlocksUntilRead(t *testing.T) {
	synctest.Test(t, func(test *testing.T) {
		const maxSize uint = 512
		reader, writer := usvc_io.NewBufferedPipeWithMaxSize(maxSize)
		defer reader.Close()
		defer writer.Close()
		count, writeErr := writer.Write(make([]byte, int(maxSize)))
		require.NoError(test, writeErr)
		require.Equal(test, int(maxSize), count)

		writeCompleted := make(chan error, 1)
		written := 0
		go func() {
			var pendingWriteErr error
			written, pendingWriteErr = writer.Write([]byte("extra"))
			writeCompleted <- pendingWriteErr
		}()
		synctest.Wait()
		select {
		case pendingWriteErr := <-writeCompleted:
			test.Fatalf("writer did not block at capacity: %v", pendingWriteErr)
		default:
		}

		buffer := make([]byte, 100)
		count, readErr := reader.Read(buffer)
		require.NoError(test, readErr)
		require.Equal(test, len(buffer), count)
		synctest.Wait()
		select {
		case pendingWriteErr := <-writeCompleted:
			require.NoError(test, pendingWriteErr)
			require.Equal(test, len("extra"), written)
		default:
			test.Fatal("writer remained blocked after buffer space was released")
		}
	})
}

// Verifies that an unlimited pipe accepts a large write without requiring concurrent reads.
func TestBufferedPipeUnlimitedWhenMaxSizeZero(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
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
	case <-ctx.Done():
		t.Fatal("Write should not block when maxSize is 0 (unlimited)")
	}
}
