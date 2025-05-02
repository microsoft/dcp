package io

import (
	"bytes"
	"io"
	"time"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

// TimestampWriter is an WriteSyncerCloser that wraps another writer
// and appends timestamps before the first content of each new line.
type timestampWriter struct {
	// The underlying writer to write to
	inner WriteSyncerCloser
	// Do we need to write a timestamp before we output the next byte?
	needsTimestamp bool
	// Buffer for processing output
	buffer *bytes.Buffer
	closed bool
}

// NewTimestampWriter creates a new TimestampWriter that wraps the given writer and adds timestamps to the written data.
func NewTimestampWriter(inner WriteSyncerCloser) WriteSyncerCloser {
	return &timestampWriter{
		inner:          inner,
		needsTimestamp: true,
		buffer:         new(bytes.Buffer),
	}
}

// Writes the given bytes, appending a timestamp in RFC3339 format before the first content of each new line.
func (tw *timestampWriter) Write(p []byte) (int, error) {
	if tw.closed {
		return 0, ErrClosedWriter
	}

	// Reset the buffer before every read
	reset(&tw.buffer)

	// Note: buffer writes never return an error (they may panic if the buffer grows beyond 2GB)

	for _, b := range p {
		if tw.needsTimestamp {
			_, _ = tw.buffer.WriteString(time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat) + " ")
			tw.needsTimestamp = false
		}

		if b == '\n' {
			tw.needsTimestamp = true
		}

		tw.buffer.WriteByte(b)
	}

	data := tw.buffer.Bytes()

	n, err := tw.inner.Write(data)
	if err != nil {
		return n, err
	}

	if n != len(data) {
		return n, io.ErrShortWrite
	}

	// Return the original number of bytes we were expected to write to avoid triggering
	// a short write error in the caller.
	return len(p), nil
}

func (tw *timestampWriter) Close() error {
	if tw.closed {
		return nil
	}
	tw.closed = true
	return tw.inner.Close()
}

func (tw *timestampWriter) Sync() error {
	return tw.inner.Sync()
}
