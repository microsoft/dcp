/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"context"
	"io"
	"sync"
)

// CancellableReader wraps an io.Reader and allows individual reads to be cancelled.
//
// CancellableReader is not goroutine-safe.
//
// The buffer returned from Read() is valid only until the next Read() call.
// If data needs to be preserved beyond the next Read() call, callers must copy it to a different buffer.
//
// Callers must check the returned buffer even when Read() returns an error--some data may be returned
// even if read error occurs.
type CancellableReader struct {
	inner       io.Reader
	lifetimeCtx context.Context
	buffer      []byte
	incoming    chan []byte
	outgoing    chan cancellableReaderResult

	startWorkerOnce func()
	readPending     bool
}

type cancellableReaderResult struct {
	n   int
	err error
}

// NewCancellableReader creates a CancellableReader around the provided reader.
func NewCancellableReader(lifetimeCtx context.Context, reader io.Reader, maxReadSize int) *CancellableReader {
	if lifetimeCtx == nil {
		panic("lifetime context must not be nil")
	}
	if reader == nil {
		panic("reader must not be nil")
	}
	if maxReadSize <= 0 {
		panic("maxReadSize must be greater than 0")
	}

	cr := CancellableReader{
		inner:       reader,
		lifetimeCtx: lifetimeCtx,
		buffer:      make([]byte, maxReadSize),
		incoming:    make(chan []byte),
		outgoing:    make(chan cancellableReaderResult, 1),
	}
	cr.startWorkerOnce = sync.OnceFunc(func() {
		go cr.doWork()
	})

	return &cr
}

func (cr *CancellableReader) doWork() {
	for {
		select {
		case buf, isOpen := <-cr.incoming:
			if cr.lifetimeCtx.Err() != nil || !isOpen {
				return
			}

			n, readErr := cr.inner.Read(buf)

			select {
			case cr.outgoing <- cancellableReaderResult{n: n, err: readErr}:
			case <-cr.lifetimeCtx.Done():
				return
			}

		case <-cr.lifetimeCtx.Done():
			return
		}
	}
}

// Read reads from the wrapped reader using the CancellableReader's internal buffer.
func (cr *CancellableReader) Read(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		panic("context must not be nil")
	}

	if lifetimeErr := cr.lifetimeCtx.Err(); lifetimeErr != nil {
		return nil, lifetimeErr
	}

	if cr.readPending {
		return cr.waitForResult(ctx)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	cr.startWorkerOnce()

	select {
	case cr.incoming <- cr.buffer:
		cr.readPending = true
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-cr.lifetimeCtx.Done():
		return nil, cr.lifetimeCtx.Err()
	}

	return cr.waitForResult(ctx)
}

func (cr *CancellableReader) waitForResult(ctx context.Context) ([]byte, error) {
	select {
	case result := <-cr.outgoing:
		cr.readPending = false
		return cr.buffer[:result.n], result.err

	case <-ctx.Done():
		// If we also get the result, return it instead of the context error
		select {
		case result := <-cr.outgoing:
			cr.readPending = false
			return cr.buffer[:result.n], result.err
		default:
			return nil, ctx.Err()
		}

	case <-cr.lifetimeCtx.Done():
		// If we also get the result, return it instead of the context error
		select {
		case result := <-cr.outgoing:
			cr.readPending = false
			return cr.buffer[:result.n], result.err
		default:
			return nil, cr.lifetimeCtx.Err()
		}
	}
}
