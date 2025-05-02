package io

import (
	"bufio"
	"bytes"
	"io"
	"time"
	"unicode"
)

// Reasonable length longer than any RFC3339Nano timestamp
const maxTimestampLength = 40

type TimestampAwareReaderOptions struct {
	Timestamps bool
	Limit      int64
	Skip       int64
}

type timestampAwareReader struct {
	inner           io.Reader
	reader          *bufio.Reader
	opts            TimestampAwareReaderOptions
	maybeTimestamp  bool
	skippedLines    int64
	writtenLines    int64
	timestampBuffer *bytes.Buffer
}

func NewTimestampAwareReader(inner io.Reader, opts TimestampAwareReaderOptions) io.ReadCloser {
	retval := timestampAwareReader{
		inner:           inner,
		reader:          bufio.NewReader(inner),
		opts:            opts,
		skippedLines:    0,
		writtenLines:    0,
		timestampBuffer: new(bytes.Buffer),
	}
	if opts.Timestamps {
		retval.maybeTimestamp = false
	} else {
		retval.maybeTimestamp = true
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

	for {
		if written == bufSize {
			break
		}

		if tr.maybeTimestamp {
			b, readErr = tr.reader.ReadByte()
			if readErr != nil {
				// Do not clear the buffer and do not change maybeTimestamp, allowing us to properly resume when new data is available
				return written, readErr
			}

			_ = tr.timestampBuffer.WriteByte(b) // WriteByte() never fails (only panics if memory is exhausted)

			switch {

			case b == '\r' || b == '\n' || tr.timestampBuffer.Len() > maxTimestampLength:
				// Timestamp (if any) is not separated by whitespace, just write out whatever we buffered
				tr.maybeTimestamp = false

			case unicode.IsSpace(rune(b)):
				// Parsing RFC3339 also handles versions of RFC3339 with sub-second precision, so we only do the one check.
				// This code specifically doesn't handle non-RFC3339 timestamps as those can include spaces,
				// and would be too difficult to properly parse. Currently all log sources write in RFC3339 format,
				// but if that changes, we would need to special case that scenario.

				// Try parsing what we buffered, minus the last whitespace character
				_, timeParseErr := time.Parse(time.RFC3339, string(tr.timestampBuffer.Bytes()[:tr.timestampBuffer.Len()-1]))
				if timeParseErr == nil {
					// Throw away the timestamp and the space and proceed with the rest of the line
					tr.timestampBuffer.Reset()
				}
				// If we hit an error parsing the timestamp, we assume it's not a timestamp and just write it out.

				tr.maybeTimestamp = false

			}
		}

		if !tr.maybeTimestamp {
			// If we have buffered data, write it out, otherwise read from the inner stream.
			if tr.timestampBuffer.Len() > 0 {
				b, _ = tr.timestampBuffer.ReadByte() // The only possible error is EOF (if the buffer is empty)
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
					break // Limit of lines to read reached.
				}

				if !tr.opts.Timestamps && tr.timestampBuffer.Len() <= 0 {
					tr.maybeTimestamp = true
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
