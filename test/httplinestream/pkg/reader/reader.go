// Copyright (c) Microsoft Corporation. All rights reserved.

package reader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/test/httplinestream/pkg/line"
	"github.com/microsoft/dcp/test/httplinestream/pkg/stream"
)

// StreamSource defines the source of the stream
type StreamSource int

const (
	// StreamSourceHTTP retrieves the stream from an HTTP server
	StreamSourceHTTP StreamSource = iota
	// StreamSourceLocal creates a local stream
	StreamSourceLocal
)

// ReaderOptions configures the reader
type ReaderOptions struct {
	BatchSize    int
	ChunkSize    int
	Delay        time.Duration
	FillBuffer   bool
	StreamSource StreamSource
}

// DefaultReaderOptions returns default options
func DefaultReaderOptions() *ReaderOptions {
	return &ReaderOptions{
		BatchSize:    100,
		ChunkSize:    4_000_000,
		Delay:        15 * time.Millisecond,
		FillBuffer:   true,
		StreamSource: StreamSourceHTTP,
	}
}

// Reader reads and validates lines from a stream
type Reader struct {
	httpClient    *http.Client
	logger        *logr.Logger
	options       *ReaderOptions
	baseURL       string
	localStreamCh chan struct{}
}

// NewReader creates a new reader
func NewReader(client *http.Client, logger *logr.Logger, options *ReaderOptions, baseURL string) *Reader {
	return &Reader{
		httpClient:    client,
		logger:        logger,
		options:       options,
		baseURL:       baseURL,
		localStreamCh: make(chan struct{}),
	}
}

// GetStream returns a stream based on the configured source
func (r *Reader) GetStream(ctx context.Context) (io.ReadCloser, error) {
	switch r.options.StreamSource {
	case StreamSourceHTTP:
		req, err := http.NewRequestWithContext(ctx, "GET", r.baseURL+"/values.csv", nil)
		if err != nil {
			return nil, err
		}

		resp, err := r.httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		return resp.Body, nil

	case StreamSourceLocal:
		localStream := stream.NewReadWriteStream()

		// Start writing to the stream
		done := make(chan struct{})
		go func() {
			defer close(r.localStreamCh)
			logger := r.logger
			writeOpts := line.NewLocalWriteOptions(logger)
			line.WriteLinesAsync(localStream, writeOpts, done)
			localStream.SetEndOfStream(true)
		}()

		go func() {
			<-ctx.Done()
			close(done)
		}()

		return io.NopCloser(localStream), nil

	default:
		return nil, errors.New("invalid stream source")
	}
}

// ReadUntilFullAsync reads from the stream until the buffer is full or the stream ends
func ReadUntilFullAsync(ctx context.Context, stream io.Reader, buffer []byte) (int, error) {
	count := 0
	for count < len(buffer) {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		read, err := stream.Read(buffer[count:])
		if err != nil {
			if err == io.EOF {
				return count, nil
			}
			return count, err
		}
		if read == 0 {
			break
		}
		count += read
	}
	return count, nil
}

// ReadOnceAsync reads from the stream once
func ReadOnceAsync(stream io.Reader, buffer []byte) (int, error) {
	return stream.Read(buffer)
}

// ProcessLine validates a line against the expected value
func (r *Reader) ProcessLine(lineData []byte, row int64, bytesConsumed int64) error {
	if len(lineData) == 0 {
		return nil
	}

	expected := line.FormatLine(row, false)
	if !bytes.Equal(lineData, expected) {
		err := fmt.Errorf("reader line was corrupted at row %d, ~%d bytes", row, bytesConsumed)
		r.logger.Error(err, "Read corrupted line", "Row", row, "BytesConsumed", bytesConsumed)
		return err
	}
	return nil
}

// ProcessBatch simulates batch processing to help trigger the issue
func (r *Reader) ProcessBatch() error {
	time.Sleep(r.options.Delay)
	return nil
}

// ReadStream reads and processes the stream
func (r *Reader) ReadStream(ctx context.Context, stream io.Reader) error {
	buffer := make([]byte, r.options.ChunkSize)
	bytesConsumed := int64(0)
	offset := 0
	row := int64(0)

	for {
		var length int
		var err error

		if r.options.FillBuffer {
			length, err = ReadUntilFullAsync(ctx, stream, buffer[offset:])
		} else {
			length, err = ReadOnceAsync(stream, buffer[offset:])
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		if offset+length == 0 {
			break
		}

		data := buffer[:offset+length]
		for {
			if ctx.Err() != nil {
				// Exit early if context is done
				return nil
			}
			idx := bytes.IndexByte(data, '\n')
			if idx == -1 {
				break
			}

			line := data[:idx]
			data = data[idx+1:]
			bytesConsumed += int64(len(line) + 1)

			err = r.ProcessLine(line, row, bytesConsumed)
			if err != nil {
				return err
			}
			row++

			if row%10_000 == 0 {
				r.logger.Info("Reader: Processed lines", "count", row, "bytesConsumed", bytesConsumed)
			}
		}

		offset = len(data)
		if offset > 0 {
			copy(buffer, data)
		}

		// Process batch if needed
		if row > 0 && row%int64(r.options.BatchSize) == 0 {
			err = r.ProcessBatch()
			if err != nil {
				return err
			}
		}

		if err == io.EOF {
			break
		}
	}

	return nil
}

// Close cleans up resources
func (r *Reader) Close() error {
	if r.options.StreamSource == StreamSourceLocal {
		// Wait for local stream to finish
		<-r.localStreamCh
	}
	return nil
}
