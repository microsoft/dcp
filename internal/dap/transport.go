// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/google/go-dap"
)

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

// tcpTransport implements Transport over a TCP connection.
type tcpTransport struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer

	// writeMu protects concurrent writes to the connection
	writeMu sync.Mutex

	// closed indicates whether the transport has been closed
	closed bool
	mu     sync.Mutex
}

// NewTCPTransport creates a new Transport backed by a TCP connection.
func NewTCPTransport(conn net.Conn) Transport {
	return &tcpTransport{
		conn:   conn,
		reader: bufio.NewReader(conn),
		writer: bufio.NewWriter(conn),
	}
}

// DialTCP establishes a TCP connection to the specified address and returns a Transport.
func DialTCP(ctx context.Context, address string) (Transport, error) {
	var d net.Dialer
	conn, dialErr := d.DialContext(ctx, "tcp", address)
	if dialErr != nil {
		return nil, fmt.Errorf("failed to dial TCP %s: %w", address, dialErr)
	}

	return NewTCPTransport(conn), nil
}

func (t *tcpTransport) ReadMessage() (dap.Message, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("transport is closed")
	}
	t.mu.Unlock()

	msg, readErr := dap.ReadProtocolMessage(t.reader)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read DAP message: %w", readErr)
	}

	return msg, nil
}

func (t *tcpTransport) WriteMessage(msg dap.Message) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("transport is closed")
	}
	t.mu.Unlock()

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	writeErr := dap.WriteProtocolMessage(t.writer, msg)
	if writeErr != nil {
		return fmt.Errorf("failed to write DAP message: %w", writeErr)
	}

	flushErr := t.writer.Flush()
	if flushErr != nil {
		return fmt.Errorf("failed to flush DAP message: %w", flushErr)
	}

	return nil
}

func (t *tcpTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}

	t.closed = true
	return t.conn.Close()
}

// stdioTransport implements Transport over stdin/stdout streams.
type stdioTransport struct {
	reader *bufio.Reader
	writer *bufio.Writer
	stdin  io.ReadCloser
	stdout io.WriteCloser

	// writeMu protects concurrent writes
	writeMu sync.Mutex

	// closed indicates whether the transport has been closed
	closed bool
	mu     sync.Mutex
}

// NewStdioTransport creates a new Transport backed by stdin and stdout streams.
// The caller is responsible for ensuring that stdin supports reading and stdout supports writing.
func NewStdioTransport(stdin io.ReadCloser, stdout io.WriteCloser) Transport {
	return &stdioTransport{
		reader: bufio.NewReader(stdin),
		writer: bufio.NewWriter(stdout),
		stdin:  stdin,
		stdout: stdout,
	}
}

func (t *stdioTransport) ReadMessage() (dap.Message, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, fmt.Errorf("transport is closed")
	}
	t.mu.Unlock()

	msg, readErr := dap.ReadProtocolMessage(t.reader)
	if readErr != nil {
		return nil, fmt.Errorf("failed to read DAP message: %w", readErr)
	}

	return msg, nil
}

func (t *stdioTransport) WriteMessage(msg dap.Message) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("transport is closed")
	}
	t.mu.Unlock()

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	writeErr := dap.WriteProtocolMessage(t.writer, msg)
	if writeErr != nil {
		return fmt.Errorf("failed to write DAP message: %w", writeErr)
	}

	flushErr := t.writer.Flush()
	if flushErr != nil {
		return fmt.Errorf("failed to flush DAP message: %w", flushErr)
	}

	return nil
}

func (t *stdioTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}

	t.closed = true

	var errs []error
	if closeErr := t.stdin.Close(); closeErr != nil {
		errs = append(errs, fmt.Errorf("failed to close stdin: %w", closeErr))
	}
	if closeErr := t.stdout.Close(); closeErr != nil {
		errs = append(errs, fmt.Errorf("failed to close stdout: %w", closeErr))
	}

	if len(errs) > 0 {
		return errs[0] // Return first error; could enhance to return all
	}

	return nil
}
