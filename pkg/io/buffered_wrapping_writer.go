package io

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// BufferedWrappingWriter is an io.WriteCloser that wraps another writer.
// It will buffer the data until the target writer is available.
// Once the target writer is available, all buffered data will be written to it.
// Subsequent writes will be written directly to the target writer.
// All methods of the BufferedWrappingWriter are thread-safe.

type BufferedWrappingWriter struct {
	lock   *sync.Mutex
	data   *bytes.Buffer
	writer io.Writer
	closed bool
}

// Creates a new BufferedWrappingWriter.
func NewBufferedWrappingWriter() *BufferedWrappingWriter {
	return &BufferedWrappingWriter{
		lock:   &sync.Mutex{},
		data:   &bytes.Buffer{},
		closed: false,
	}
}

// Writes the given bytes to the target writer, or buffers them if the target writer is not yet available.
func (bww *BufferedWrappingWriter) Write(p []byte) (int, error) {
	bww.lock.Lock()
	defer bww.lock.Unlock()

	if bww.closed {
		return 0, ErrClosedWriter
	}

	if bww.writer != nil {
		return bww.writer.Write(p)
	} else {
		return bww.data.Write(p)
	}
}

func (bww *BufferedWrappingWriter) Close() error {
	bww.lock.Lock()
	defer bww.lock.Unlock()

	if bww.closed {
		return nil
	}

	bww.closed = true
	bww.data = nil
	closer, ok := bww.writer.(io.Closer)
	if ok {
		return closer.Close()
	}
	bww.writer = nil
	return nil
}

func (bww *BufferedWrappingWriter) SetTarget(writer io.Writer) error {
	if writer == nil {
		return fmt.Errorf("writer cannot be nil")
	}

	bww.lock.Lock()
	defer bww.lock.Unlock()

	if bww.closed {
		return ErrClosedWriter
	}

	if bww.writer != nil {
		return fmt.Errorf("target writer already set")
	}

	bww.writer = writer

	_, err := io.Copy(writer, bww.data)
	bww.data = nil
	if err != nil {
		return fmt.Errorf("failed to copy buffered data to target writer: %w", err)
	}

	return nil
}

var _ io.WriteCloser = (*BufferedWrappingWriter)(nil)
