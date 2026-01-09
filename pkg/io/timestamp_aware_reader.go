/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"time"
	"unicode"
)

const (
	maxTimestampLength = 40 // Reasonable length longer than any RFC3339Nano timestamp
	maxUint32Digits    = 10 // Maximum number of digits in a uint32
)

type TimestampAwareReaderOptions struct {
	Timestamps  bool
	Limit       int64
	Skip        int64
	LineNumbers bool
}

type timestampAwareReaderState uint

const (
	tarStateRest            timestampAwareReaderState = 0
	tarStateMaybeLineNumber timestampAwareReaderState = 1
	tarStateMaybeTimestamp  timestampAwareReaderState = 2
)

// timestampAwareReader is a reader that reads from an inner reader and detects line numbers and timestamps in the data.
// Depending on the options, it will remove line numbers and/or timestamps from the returned data.
// It can also skip a desired number of lines at the beginning of the stream and limit the number of lines returned.
type timestampAwareReader struct {
	inner            io.Reader
	reader           *bufio.Reader
	opts             TimestampAwareReaderOptions
	state            timestampAwareReaderState
	skippedLines     int64
	writtenLines     int64
	timestampBuffer  *bytes.Buffer
	lineNumberBuffer *bytes.Buffer
}

func NewTimestampAwareReader(inner io.Reader, opts TimestampAwareReaderOptions) io.ReadCloser {
	retval := timestampAwareReader{
		inner:            inner,
		reader:           bufio.NewReader(inner),
		opts:             opts,
		skippedLines:     0,
		writtenLines:     0,
		timestampBuffer:  new(bytes.Buffer),
		lineNumberBuffer: new(bytes.Buffer),
	}

	if opts.Timestamps && opts.LineNumbers {
		retval.state = tarStateRest // We will be returning everything as-is and never switching state.
	} else {
		retval.state = tarStateMaybeLineNumber
	}

	return &retval
}

func (tr *timestampAwareReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, io.ErrShortBuffer
	}

	if tr.opts.Limit > 0 && tr.writtenLines == tr.opts.Limit {
		return 0, io.EOF
	}

	skipErr := tr.skipIfRequested()
	if skipErr != nil {
		return 0, skipErr
	}

	bufSize := len(p)
	written := 0
	var b byte
	var readErr error
	var skipNextRead bool

	tryReinterpretingAsTimestamp := func() {
		_, _ = tr.timestampBuffer.Write(tr.lineNumberBuffer.Bytes())
		tr.lineNumberBuffer.Reset()
		tr.state = tarStateMaybeTimestamp
		skipNextRead = true
	}

readLoop:
	for {
		if written == bufSize {
			break readLoop
		}

		switch tr.state {

		// Trying to read a line number from the input stream
		case tarStateMaybeLineNumber:

			b, readErr = tr.reader.ReadByte()
			if readErr != nil {
				// Do not clear buffers and do not change state, allowing us to properly resume when new data is available
				return written, readErr
			}

			_ = tr.lineNumberBuffer.WriteByte(b)

			if b == '\n' || b == '\r' || tr.lineNumberBuffer.Len() > (maxUint32Digits+1) {
				// Line number is not separated by whitespace, or it is too long.
				if !tr.opts.Timestamps {
					tryReinterpretingAsTimestamp()
				} else {
					tr.state = tarStateRest // Just write out the line as-is.
				}
				continue
			}

			if unicode.IsSpace(rune(b)) {
				// Try to parse the line number.
				_, lineNumberErr := strconv.ParseUint(string(tr.lineNumberBuffer.Bytes()[:tr.lineNumberBuffer.Len()-1]), 10, 32)

				if lineNumberErr == nil {
					if !tr.opts.LineNumbers {
						tr.lineNumberBuffer.Reset() // We do not want it in the output
					}

					if tr.opts.Timestamps {
						tr.state = tarStateRest // Not looking for timestamps, just write out the rest of the line
					} else {
						tr.state = tarStateMaybeTimestamp
					}
				} else {
					// We were looking for a line number, but that is not what we encountered.
					if !tr.opts.Timestamps {
						tryReinterpretingAsTimestamp()
					} else {
						tr.state = tarStateRest // Just write out the line as-is.
					}
				}
			}

		// Trying to read a timestamp from the input stream
		case tarStateMaybeTimestamp:

			if skipNextRead {
				skipNextRead = false
			} else {
				b, readErr = tr.reader.ReadByte()
				if readErr != nil {
					return written, readErr
				}

				_ = tr.timestampBuffer.WriteByte(b)
			}

			if b == '\r' || b == '\n' || tr.timestampBuffer.Len() > maxTimestampLength {
				// Timestamp is not separated by whitespace, or it is too long.
				tr.state = tarStateRest
				continue
			}

			if unicode.IsSpace(rune(b)) {
				// Parsing RFC3339 also handles versions of RFC3339 with sub-second precision, so we only do the one check.
				// This code specifically doesn't handle non-RFC3339 timestamps as those can include spaces,
				// and would be too difficult to properly parse. Currently all log sources write in RFC3339 format,
				// but if that changes, we would need to special case that scenario.

				// Try parsing what we buffered, minus the last whitespace character
				_, timeParseErr := time.Parse(time.RFC3339, string(tr.timestampBuffer.Bytes()[:tr.timestampBuffer.Len()-1]))
				if timeParseErr == nil {
					if !tr.opts.Timestamps {
						tr.timestampBuffer.Reset() // We do not want it in the output
					}
					tr.state = tarStateRest
				}
			}

		// If we have buffered data, write it out, otherwise write out "the rest" of the line from the inner stream.
		default:

			if tr.lineNumberBuffer.Len() > 0 {
				b, _ = tr.lineNumberBuffer.ReadByte()
				if tr.lineNumberBuffer.Len() <= 0 {
					tr.lineNumberBuffer.Reset()
				}
			} else if tr.timestampBuffer.Len() > 0 {
				b, _ = tr.timestampBuffer.ReadByte()
				if tr.timestampBuffer.Len() <= 0 {
					tr.timestampBuffer.Reset()
				}
			} else {
				b, readErr = tr.reader.ReadByte()
				if readErr != nil {
					return written, readErr
				}
			}

			p[written] = b
			written += 1

			if b == '\n' {
				tr.writtenLines++
				if tr.opts.Limit > 0 && tr.writtenLines == tr.opts.Limit {
					break readLoop
				}

				if tr.lineNumberBuffer.Len() <= 0 && tr.timestampBuffer.Len() <= 0 {
					if !tr.opts.Timestamps || !tr.opts.LineNumbers {
						// If we finished writing a line, AND there is no buffered data,
						// AND we need to filter out line numbers or timestamps, then switch to trying to parse a line number.
						tr.state = tarStateMaybeLineNumber
					}
				}
			}
		}
	}

	return written, nil
}

func (tr *timestampAwareReader) Close() error {
	if closer, isCloser := tr.inner.(io.Closer); isCloser {
		return closer.Close()
	}

	return nil
}

func (tr *timestampAwareReader) skipIfRequested() error {
	if tr.opts.Skip > 0 {
		for tr.skippedLines < tr.opts.Skip {
			b, err := tr.reader.ReadByte()
			if err != nil {
				return err
			}
			if b == '\n' {
				tr.skippedLines++
			}
		}
	}

	return nil
}
