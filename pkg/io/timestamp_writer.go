/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"bytes"
	"io"
	"strconv"
	"time"

	"github.com/microsoft/dcp/pkg/osutil"
)

const (
	FirstLogLineNumber = 1
)

// TimestampWriter is an WriteSyncerCloser that wraps another writer
// and appends line numbers and timestamps before the first content of each new line.
// TimestampWriter is not thread-safe.
type timestampWriter struct {
	// The underlying writer to write to
	inner WriteSyncerCloser
	// Do we need to write a line number + timestamp before we output the next byte?
	needsTimestamp bool
	// The current line number (1-based)
	line uint32
	// Buffer for processing output
	buffer *bytes.Buffer
	closed bool
}

// Creates a new TimestampWriter that wraps the given writer.
func NewTimestampWriter(inner WriteSyncerCloser) WriteSyncerCloser {
	return &timestampWriter{
		inner:          inner,
		needsTimestamp: true,
		line:           FirstLogLineNumber,
		buffer:         new(bytes.Buffer),
	}
}

// Writes the given bytes, adding a line number and a timestamp in RFC3339 format before the first byte of each new line.
func (tw *timestampWriter) Write(p []byte) (int, error) {
	if tw.closed {
		return 0, ErrClosedWriter
	}

	reset(&tw.buffer)

	// Note: buffer writes never return an error (they may panic if the buffer grows beyond 2GB)

	for _, b := range p {
		if tw.needsTimestamp {
			lineBytes := strconv.AppendUint(nil, uint64(tw.line), 10)
			tw.line++
			_, _ = tw.buffer.Write(lineBytes)
			_, _ = tw.buffer.WriteString(" ")
			_, _ = tw.buffer.WriteString(time.Now().UTC().Format(osutil.RFC3339MiliTimestampFormat))
			_, _ = tw.buffer.WriteString(" ")
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
