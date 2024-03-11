package testutil

import (
	"bytes"
	"io"
	"math"
	"sync"
	"time"
)

// BufferWriter is a simple implementation of io.Writer that writes to a (dynamically expanding) buffer.
// Writes succeed until the buffer is full.
// All methods are goroutine-safe.
// Every write operation is tracked and timestamped, and the written data with associated timestamps
// can be retrieved using Chunks() method.

type Chunk struct {
	Offset    int
	Length    int
	Timestamp time.Time
}

type BufferWriter struct {
	data    []byte
	chunks  []Chunk
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

	bw.chunks = append(bw.chunks, Chunk{
		Offset:    len(bw.data),
		Length:    len(p),
		Timestamp: time.Now(),
	})
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

func (bw *BufferWriter) Chunks() []Chunk {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	if bw.chunks == nil {
		return nil
	}
	return append([]Chunk{}, bw.chunks...) // make a copy
}

func (bw *BufferWriter) ChunksLen() int {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	return len(bw.chunks)
}

var _ io.WriteCloser = (*BufferWriter)(nil)
