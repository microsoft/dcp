package testutil

import (
	"bytes"
	"io"
	"math"
	"sync"
)

// BufferWriter is a simple implementation of io.Writer that writes to a (dynamically expanding) buffer.
// Writes succeed until the buffer is full.
// All methods are goroutine-safe.

type BufferWriter struct {
	data    []byte
	lock    *sync.Mutex
	maxSize uint
	closed  bool
}

func NewBufferWriter(maxSize uint) *BufferWriter {
	return &BufferWriter{
		lock:    &sync.Mutex{},
		maxSize: maxSize,
		closed:  false,
	}
}

func (bw *BufferWriter) Write(p []byte) (n int, err error) {
	bw.lock.Lock()
	defer bw.lock.Unlock()

	if bw.closed || uint(len(bw.data)) > math.MaxUint-uint(len(p)) {
		return 0, io.ErrShortWrite
	}

	bw.data = append(bw.data, p...)
	return len(p), nil
}

func (bw *BufferWriter) Bytes() []byte {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	return bytes.Clone(bw.data)
}

func (bw *BufferWriter) Close() error {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	bw.closed = true
	return nil
}

var _ io.WriteCloser = (*BufferWriter)(nil)
