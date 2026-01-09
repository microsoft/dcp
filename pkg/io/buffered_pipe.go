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
	lock *sync.Mutex
	cond *sync.Cond
	data *bytes.Buffer
	rerr error
	werr error
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
			return bpr.data.Read(p)
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

	n, err := bpw.data.Write(p)
	bpw.cond.Signal()
	return n, err
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
	p := bufferedPipe{
		data: new(bytes.Buffer),
		lock: new(sync.Mutex),
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
