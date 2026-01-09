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

// ContextReader is a reader that will read from an inner reader until the context is cancelled.
type ContextReader struct {
	inner           io.Reader
	ctx             context.Context
	incoming        chan []byte
	outgoing        chan contextReaderResult
	startWorkerOnce func()
}

type contextReaderResult struct {
	n   int
	err error
}

// Creates a new ContextReader instance
// If leverageReadCloser is true, the ContextReader will take advantage of the fact that the reader is also a ReadCloser
// and will call Close() on the reader when the context is cancelled.
// This allows the ContextReader to work without extra worker goroutine.
// The user of the ContextReader must ensure that the reader is not closed by other means.
func NewContextReader(ctx context.Context, r io.Reader, leverageReadCloser bool) *ContextReader {
	cr := ContextReader{
		inner: r,
		ctx:   ctx,
	}

	rc, isReadCloser := r.(io.ReadCloser)
	if isReadCloser && leverageReadCloser {
		context.AfterFunc(ctx, func() { _ = rc.Close() })
	} else {
		cr.incoming = make(chan []byte)
		cr.outgoing = make(chan contextReaderResult)
		cr.startWorkerOnce = sync.OnceFunc(func() {
			go cr.doWork()
		})
	}

	return &cr
}

func (cr *ContextReader) doWork() {
	for {
		select {
		case buf, isOpen := <-cr.incoming:
			if cr.ctx.Err() != nil || !isOpen {
				return
			}

			n, err := cr.inner.Read(buf)

			select {
			case cr.outgoing <- contextReaderResult{n: n, err: err}:
			case <-cr.ctx.Done():
				return // Exit the worker goroutine when the context is cancelled
			}

		case <-cr.ctx.Done():
			return // No more work
		}
	}
}

func (cr *ContextReader) Read(p []byte) (int, error) {
	if cr.ctx.Err() != nil {
		return 0, cr.ctx.Err()
	}

	if cr.startWorkerOnce == nil {
		// In io.ReadCloser mode
		return cr.inner.Read(p)
	}

	cr.startWorkerOnce()

	select {
	case cr.incoming <- p:
	case <-cr.ctx.Done():
		return 0, cr.ctx.Err()
	}

	select {
	case res := <-cr.outgoing:
		return res.n, res.err
	case <-cr.ctx.Done():
		return 0, cr.ctx.Err()
	}
}

var _ io.Reader = (*ContextReader)(nil)
