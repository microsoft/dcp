/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/google/go-dap"
	dcpio "github.com/microsoft/dcp/pkg/io"
)

// ErrTransportClosed is returned when a read or write is attempted on a transport
// that has been intentionally closed via Close(). This distinguishes expected
// shutdown errors from unexpected connection failures.
var ErrTransportClosed = errors.New("transport closed")

// Transport provides an abstraction for DAP message I/O over different connection types.
// Implementations must be safe for concurrent use by multiple goroutines for reading
// and writing, but individual reads and writes may not be concurrent with each other.
type Transport interface {
	// ReadMessage reads the next DAP protocol message from the transport.
	// Returns the message or an error if reading fails.
	// This method blocks until a complete message is available.
	ReadMessage() (dap.Message, error)

	// WriteMessage writes a DAP protocol message to the transport.
	// Returns an error if writing fails.
	WriteMessage(msg dap.Message) error

	// Close closes the transport, releasing any associated resources.
	// After Close is called, any blocked ReadMessage or WriteMessage calls
	// should return with an error.
	Close() error
}

// connTransport implements Transport over any connection that provides
// an io.Reader for incoming data and an io.Writer for outgoing data.
// It is used for TCP, Unix domain socket, and stdio-based transports.
type connTransport struct {
	reader *bufio.Reader
	writer *bufio.Writer
	closer io.Closer

	// closed tracks whether Close() has been called. This is used to wrap
	// subsequent read/write errors with ErrTransportClosed so callers can
	// distinguish intentional shutdown from unexpected failures.
	closed atomic.Bool

	// writeMu serializes message writes. Each DAP message is sent as a
	// content-length header followed by the message body in separate writes,
	// then flushed. The mutex ensures this multi-write sequence is atomic
	// so concurrent WriteMessage calls cannot interleave their bytes.
	writeMu sync.Mutex
}

// NewTCPTransportWithContext creates a new Transport backed by a TCP connection
// that respects context cancellation. When the context is cancelled, any blocked
// reads will be unblocked by closing the connection.
func NewTCPTransportWithContext(ctx context.Context, conn net.Conn) Transport {
	return newConnTransport(ctx, conn, conn, conn)
}

// NewStdioTransportWithContext creates a new Transport backed by stdin and stdout streams
// that respects context cancellation. When the context is cancelled, any blocked
// reads will be unblocked by closing the stdin stream.
func NewStdioTransportWithContext(ctx context.Context, stdin io.ReadCloser, stdout io.WriteCloser) Transport {
	return newConnTransport(ctx, stdin, stdout, multiCloser{stdin, stdout})
}

// NewUnixTransportWithContext creates a new Transport backed by a Unix domain socket connection
// that respects context cancellation. When the context is cancelled, any blocked
// reads will be unblocked by closing the connection.
func NewUnixTransportWithContext(ctx context.Context, conn net.Conn) Transport {
	return newConnTransport(ctx, conn, conn, conn)
}

// newConnTransport creates a connTransport from separate read, write, and close resources.
// A ContextReader wraps the reader so that context cancellation unblocks pending reads.
func newConnTransport(ctx context.Context, r io.Reader, w io.Writer, closer io.Closer) Transport {
	contextReader := dcpio.NewContextReader(ctx, r, true)
	return &connTransport{
		reader: bufio.NewReader(contextReader),
		writer: bufio.NewWriter(w),
		closer: closer,
	}
}

func (t *connTransport) ReadMessage() (dap.Message, error) {
	msg, readErr := ReadMessageWithFallback(t.reader)
	if readErr != nil {
		if t.closed.Load() {
			return nil, fmt.Errorf("%w: %w", ErrTransportClosed, readErr)
		}
		return nil, fmt.Errorf("failed to read DAP message: %w", readErr)
	}

	return msg, nil
}

func (t *connTransport) WriteMessage(msg dap.Message) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	writeErr := WriteMessageWithFallback(t.writer, msg)
	if writeErr != nil {
		if t.closed.Load() {
			return fmt.Errorf("%w: %w", ErrTransportClosed, writeErr)
		}
		return fmt.Errorf("failed to write DAP message: %w", writeErr)
	}

	flushErr := t.writer.Flush()
	if flushErr != nil {
		if t.closed.Load() {
			return fmt.Errorf("%w: %w", ErrTransportClosed, flushErr)
		}
		return fmt.Errorf("failed to flush DAP message: %w", flushErr)
	}

	return nil
}

func (t *connTransport) Close() error {
	t.closed.Store(true)
	return t.closer.Close()
}

// isExpectedShutdownErr returns true if the error is expected during normal
// bridge shutdown — for example, when transports are intentionally closed,
// the context is cancelled, or the remote end disconnects cleanly.
func isExpectedShutdownErr(err error) bool {
	return errors.Is(err, ErrTransportClosed) ||
		errors.Is(err, context.Canceled) ||
		isExpectedCloseErr(err)
}

// multiCloser closes multiple io.Closers, returning the first error.
type multiCloser []io.Closer

func (mc multiCloser) Close() error {
	var firstErr error
	for _, c := range mc {
		if closeErr := c.Close(); closeErr != nil && firstErr == nil {
			firstErr = closeErr
		}
	}
	return firstErr
}
