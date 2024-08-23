package io

import (
	"bytes"
	"io"
	"time"
)

const (
	timestampFormat = "2006-01-02T15:04:05.000Z07:00" // RFC3339 with milliseconds, fixed width
)

// TimestampWriter is an io.WriteCloser that wraps another writer and appends timestamps before the first content
// of each new line. Doesn't append timestamps to empty lines.
type timestampWriter struct {
	// The underlying writer to write to
	inner io.WriteCloser
	// Do we need to write a timestamp before we output the next byte?
	needsTimestamp bool
	// Buffer for processing output
	buffer *bytes.Buffer
	closed bool
}

// NewTimestampWriter creates a new TimestampWriter that wraps the given writer and sets needsTimestamp to true.
func NewTimestampWriter(inner io.WriteCloser) io.WriteCloser {
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
	tw.buffer.Reset()

	for _, b := range p {
		if tw.needsTimestamp {
			if _, err := tw.buffer.WriteString(time.Now().UTC().Format(timestampFormat) + " "); err != nil {
				return 0, err
			}
			tw.needsTimestamp = false
		}

		if b == '\n' {
			tw.needsTimestamp = true
			if err := tw.buffer.WriteByte(b); err != nil {
				return 0, err
			}
			continue
		}

		if err := tw.buffer.WriteByte(b); err != nil {
			return 0, err
		}
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

var _ io.WriteCloser = (*timestampWriter)(nil)
