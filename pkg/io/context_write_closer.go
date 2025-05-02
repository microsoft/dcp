package io

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
)

// NotifiyWriteCloser is a WriteCloser that can notify when it is closed.
type NotifyWriteCloser interface {
	WriteSyncerCloser
	Closed() <-chan struct{}
}

// contextWriteCloser is a WriteCloser that will write to an inner Writer until its context is cancelled,
// or until it is explicitly closed.
// If the inner writer is also a Closer, it will be closed when the contextWriteCloser is closed.
// contextWriteCloser is safe for concurrent use as long as the inner writer is.
type contextWriteCloser struct {
	inner    WriteSyncerCloser
	ctx      context.Context
	lock     *sync.Mutex
	closeErr error
	closed   *atomic.Bool
	closedCh chan struct{}
}

func NewContextWriteCloser(ctx context.Context, w WriteSyncerCloser) NotifyWriteCloser {
	if ctx == nil {
		panic("context must not be nil")
	}

	lock := &sync.Mutex{}

	cwc := &contextWriteCloser{
		inner:    w,
		ctx:      ctx,
		lock:     lock,
		closeErr: nil,
		closed:   &atomic.Bool{},
		closedCh: make(chan struct{}),
	}

	context.AfterFunc(ctx, func() {
		_ = cwc.Close()
	})

	return cwc
}

func (cwc *contextWriteCloser) Write(p []byte) (int, error) {
	ctxErr := cwc.ctx.Err()
	if ctxErr != nil {
		return 0, ctxErr
	}

	if cwc.closed.Load() {
		return 0, ErrClosedWriter
	}

	// Technically the writer can get closed between the check and the write,
	// but the worst case we will get is a write error, which will be reported to the caller.
	return cwc.inner.Write(p)
}

func (cwc *contextWriteCloser) Close() error {
	cwc.lock.Lock()
	defer cwc.lock.Unlock()

	if cwc.closed.Load() {
		return cwc.closeErr
	}

	cwc.closed.Store(true)
	defer close(cwc.closedCh)
	if closer, isCloser := cwc.inner.(io.Closer); isCloser {
		cwc.closeErr = closer.Close()
	}
	return cwc.closeErr
}

func (cwc *contextWriteCloser) Closed() <-chan struct{} {
	return cwc.closedCh
}

func (cwc *contextWriteCloser) Sync() error {
	return cwc.inner.Sync()
}

var _ NotifyWriteCloser = (*contextWriteCloser)(nil)
