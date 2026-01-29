/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"bytes"
	"io"
	"sync"
)

type bufferedPipe struct {
	lock    *sync.Mutex
	cond    *sync.Cond
	data    *bytes.Buffer
	maxSize int // 0 means unlimited
	rerr    error
	werr    error
}

type BufferedPipeReader struct {
	*bufferedPipe
}

func (bpr *BufferedPipeReader) Read(p []byte) (int, error) {
	bpr.lock.Lock()
	defer bpr.lock.Unlock()

	for {
		if bpr.rerr != nil {
			return 0, bpr.rerr
		}

		if bpr.data.Len() == 0 && bpr.werr != nil {
			bpr.rerr = io.EOF
			return 0, io.EOF
		}

		if bpr.data.Len() > 0 {
			n, readErr := bpr.data.Read(p)
			// Signal writers that buffer space may be available
			bpr.cond.Broadcast()
			return n, readErr
		}

		// No data, need to wait for it
		bpr.cond.Wait()
	}
}

// Closes the reader half of the pipe, subsequent readers will receive io.ErrClosedPipe.
func (bpr *BufferedPipeReader) Close() error {
	return bpr.CloseWithError(nil)
}

// Closes the reader half of the pipe, subsequent readers will receive the passed error.
// PipeReader spec requires that CloseWithError() never overwrites the previous error and always returns nil.
func (bpr *BufferedPipeReader) CloseWithError(err error) error {
	bpr.lock.Lock()
	defer bpr.lock.Unlock()

	if err == nil {
		err = io.ErrClosedPipe
	}
	if bpr.rerr == nil {
		bpr.rerr = err
	}

	// Need to wake up writers, if any, so they can see the reader is closed
	bpr.cond.Broadcast()
	return nil
}

type BufferedPipeWriter struct {
	*bufferedPipe
}

func (bpw *BufferedPipeWriter) Write(p []byte) (int, error) {
	bpw.lock.Lock()
	defer bpw.lock.Unlock()

	if bpw.werr != nil {
		return 0, bpw.werr
	}

	// If no max size is set, write all data at once
	if bpw.maxSize == 0 {
		n, writeErr := bpw.data.Write(p)
		bpw.cond.Signal()
		return n, writeErr
	}

	// With max size set, we may need to write in chunks as buffer space becomes available
	totalWritten := 0
	for len(p) > 0 {
		if bpw.werr != nil {
			return totalWritten, bpw.werr
		}

		if bpw.rerr != nil {
			return totalWritten, io.ErrClosedPipe
		}

		available := bpw.maxSize - bpw.data.Len()
		if available <= 0 {
			// Buffer is full, wait for reader to consume data
			bpw.cond.Wait()
			continue
		}

		// Write as much as we can fit
		toWrite := p
		if len(toWrite) > available {
			toWrite = p[:available]
		}

		n, writeErr := bpw.data.Write(toWrite)
		totalWritten += n
		p = p[n:]
		bpw.cond.Signal()

		if writeErr != nil {
			return totalWritten, writeErr
		}
	}

	return totalWritten, nil
}

// Closes the writer half of the pipe, subsequent writers will receive io.ErrClosedPipe.
// Subsequent readers will receive io.EOF.
func (bpw *BufferedPipeWriter) Close() error {
	return bpw.CloseWithError(nil)
}

// Closes the writer half of the pipe, subsequent writers will receive the passed error.
// Subsequent readers will receive io.EOF.
// PipeWriter spec requires that CloseWithError() never overwrites the previous error and always returns nil.
func (bpw *BufferedPipeWriter) CloseWithError(err error) error {
	bpw.lock.Lock()
	defer bpw.lock.Unlock()

	if err == nil {
		err = io.ErrClosedPipe
	}
	if bpw.werr == nil {
		bpw.werr = err
	}

	// Need to wake up readers, if any, and tell them that no further data will be coming
	bpw.cond.Broadcast()
	return nil
}

// NewBufferedPipe is like io.Pipe(), except it includes an automatically-expanding buffer,
// so writers are never blocked. It is also goroutine-safe.
// Inspiration/reference: https://github.com/golang/go/issues/28790, https://github.com/golang/go/issues/34502, https://github.com/acomagu/bufpipe
func NewBufferedPipe() (io.ReadCloser, io.WriteCloser) {
	return NewBufferedPipeWithMaxSize(0)
}

// NewBufferedPipeWithMaxSize is like NewBufferedPipe, but with an optional maximum buffer size.
// If maxSize is 0, the buffer can grow without limit (same as NewBufferedPipe).
// If maxSize is > 0, writers will block when the buffer reaches the maximum size,
// waiting for readers to consume data before more can be written.
func NewBufferedPipeWithMaxSize(maxSize int) (io.ReadCloser, io.WriteCloser) {
	p := bufferedPipe{
		data:    new(bytes.Buffer),
		lock:    new(sync.Mutex),
		maxSize: maxSize,
	}
	p.cond = sync.NewCond(p.lock)

	reader := BufferedPipeReader{
		bufferedPipe: &p,
	}
	writer := BufferedPipeWriter{
		bufferedPipe: &p,
	}
	return &reader, &writer
}

var _ io.ReadCloser = (*BufferedPipeReader)(nil)
var _ io.WriteCloser = (*BufferedPipeWriter)(nil)
