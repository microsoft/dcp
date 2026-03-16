/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-dap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

// uniqueSocketPath generates a unique, short socket path for testing.
// macOS has a ~104 character limit for Unix socket paths, so we use
// the system temp directory with a short filename.
func uniqueSocketPath(t *testing.T, suffix string) string {
	t.Helper()
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("dap-%s-%d.sock", suffix, time.Now().UnixNano()))
	t.Cleanup(func() { os.Remove(socketPath) })
	return socketPath
}

// setupTCPPair creates a connected TCP socket pair for testing.
func setupTCPPair(t *testing.T) (clientConn, serverConn net.Conn) {
	t.Helper()

	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	defer listener.Close()

	var wg sync.WaitGroup
	var acceptErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, acceptErr = listener.Accept()
	}()

	clientConn, dialErr := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, dialErr)

	wg.Wait()
	require.NoError(t, acceptErr)
	require.NotNil(t, serverConn)

	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})

	return clientConn, serverConn
}

func TestTCPTransportWriteAndReadMessage(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := setupTCPPair(t)

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	clientTransport := NewTCPTransportWithContext(ctx, clientConn)
	serverTransport := NewTCPTransportWithContext(ctx, serverConn)

	request := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "initialize",
		},
	}

	writeErr := clientTransport.WriteMessage(request)
	require.NoError(t, writeErr)

	received, readErr := serverTransport.ReadMessage()
	require.NoError(t, readErr)

	initReq, ok := received.(*dap.InitializeRequest)
	require.True(t, ok)
	assert.Equal(t, 1, initReq.Seq)
	assert.Equal(t, "initialize", initReq.Command)
}

func TestTCPTransportClosePreventsFurtherOperations(t *testing.T) {
	t.Parallel()

	clientConn, _ := setupTCPPair(t)

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	clientTransport := NewTCPTransportWithContext(ctx, clientConn)

	closeErr := clientTransport.Close()
	assert.NoError(t, closeErr)

	writeErr := clientTransport.WriteMessage(&dap.InitializeRequest{})
	assert.Error(t, writeErr)

	// Double close should not panic
	_ = clientTransport.Close()
}

// mockReadWriteCloser implements io.ReadWriteCloser for testing
type mockReadWriteCloser struct {
	reader   *bytes.Buffer
	writer   *bytes.Buffer
	closed   bool
	closeErr error
	mu       sync.Mutex
}

func newMockReadWriteCloser() *mockReadWriteCloser {
	return &mockReadWriteCloser{
		reader: bytes.NewBuffer(nil),
		writer: bytes.NewBuffer(nil),
	}
}

func (m *mockReadWriteCloser) Read(p []byte) (n int, err error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0, io.EOF
	}
	m.mu.Unlock()
	return m.reader.Read(p)
}

func (m *mockReadWriteCloser) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, io.ErrClosedPipe
	}
	return m.writer.Write(p)
}

func (m *mockReadWriteCloser) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return m.closeErr
}

func TestStdioTransportWriteAndReadMessage(t *testing.T) {
	t.Parallel()

	// Create connected pipes
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	clientTransport := NewStdioTransportWithContext(ctx, clientRead, clientWrite)
	serverTransport := NewStdioTransportWithContext(ctx, serverRead, serverWrite)

	defer clientTransport.Close()
	defer serverTransport.Close()

	// Send message from client to server
	request := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "initialize",
		},
	}

	var wg sync.WaitGroup
	wg.Add(1)

	var received dap.Message
	var readErr error

	go func() {
		defer wg.Done()
		received, readErr = serverTransport.ReadMessage()
	}()

	writeErr := clientTransport.WriteMessage(request)
	require.NoError(t, writeErr)

	wg.Wait()

	require.NoError(t, readErr)
	initReq, ok := received.(*dap.InitializeRequest)
	require.True(t, ok)
	assert.Equal(t, 1, initReq.Seq)
}

func TestStdioTransportClosePreventsFurtherOperations(t *testing.T) {
	t.Parallel()

	stdin := newMockReadWriteCloser()
	stdout := newMockReadWriteCloser()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	transport := NewStdioTransportWithContext(ctx, stdin, stdout)

	closeErr := transport.Close()
	assert.NoError(t, closeErr)

	writeErr := transport.WriteMessage(&dap.InitializeRequest{})
	assert.Error(t, writeErr)

	// Double close should be safe
	closeErr = transport.Close()
	assert.NoError(t, closeErr)
}

// setupUnixPair creates a connected Unix socket pair for testing.
func setupUnixPair(t *testing.T, suffix string) (clientConn, serverConn net.Conn) {
	t.Helper()

	socketPath := uniqueSocketPath(t, suffix)

	listener, listenErr := net.Listen("unix", socketPath)
	require.NoError(t, listenErr)
	defer listener.Close()

	var wg sync.WaitGroup
	var acceptErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, acceptErr = listener.Accept()
	}()

	clientConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)

	wg.Wait()
	require.NoError(t, acceptErr)
	require.NotNil(t, serverConn)

	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})

	return clientConn, serverConn
}

func TestUnixTransportWriteAndReadMessage(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := setupUnixPair(t, "ut-wr")

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	clientTransport := NewUnixTransportWithContext(ctx, clientConn)
	serverTransport := NewUnixTransportWithContext(ctx, serverConn)

	request := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "initialize",
		},
	}

	writeErr := clientTransport.WriteMessage(request)
	require.NoError(t, writeErr)

	received, readErr := serverTransport.ReadMessage()
	require.NoError(t, readErr)

	initReq, ok := received.(*dap.InitializeRequest)
	require.True(t, ok)
	assert.Equal(t, 1, initReq.Seq)
	assert.Equal(t, "initialize", initReq.Command)
}

func TestUnixTransportClosePreventsFurtherOperations(t *testing.T) {
	t.Parallel()

	clientConn, _ := setupUnixPair(t, "ut-cl")

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	clientTransport := NewUnixTransportWithContext(ctx, clientConn)

	closeErr := clientTransport.Close()
	assert.NoError(t, closeErr)

	writeErr := clientTransport.WriteMessage(&dap.InitializeRequest{})
	assert.Error(t, writeErr)

	// Double close should not panic
	_ = clientTransport.Close()
}

func TestUnixTransportWithContext(t *testing.T) {
	t.Parallel()

	socketPath := uniqueSocketPath(t, "ctx")

	// Create listener
	listener, listenErr := net.Listen("unix", socketPath)
	require.NoError(t, listenErr)
	defer listener.Close()

	// Accept connection in goroutine
	var serverConn net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, _ = listener.Accept()
	}()

	// Connect
	clientConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)

	wg.Wait()
	require.NotNil(t, serverConn)
	defer serverConn.Close()

	// Create transport with cancellable context
	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	clientTransport := NewUnixTransportWithContext(ctx, clientConn)

	// Start a blocking read, signalling when the goroutine is about to block
	readStarted := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		close(readStarted)
		_, _ = clientTransport.ReadMessage()
	}()

	// Wait for the read goroutine to be running before cancelling
	<-readStarted

	// Cancel context should unblock the read
	cancel()

	select {
	case <-readDone:
		// Success - read was unblocked
	case <-time.After(2 * time.Second):
		t.Fatal("read was not unblocked after context cancellation")
	}
}

func TestIsExpectedShutdownErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"arbitrary error", fmt.Errorf("something went wrong"), false},
		{"ErrTransportClosed", ErrTransportClosed, true},
		{"wrapped ErrTransportClosed", fmt.Errorf("failed to read: %w", ErrTransportClosed), true},
		{"context.Canceled", context.Canceled, true},
		{"wrapped context.Canceled", fmt.Errorf("read failed: %w", context.Canceled), true},
		{"io.EOF", io.EOF, true},
		{"wrapped io.EOF", fmt.Errorf("read: %w", io.EOF), true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"wrapped net.ErrClosed", fmt.Errorf("read: %w", net.ErrClosed), true},
		{"io.ErrClosedPipe", io.ErrClosedPipe, true},
		{"wrapped io.ErrClosedPipe", fmt.Errorf("write: %w", io.ErrClosedPipe), true},
		{"double wrapped ErrTransportClosed", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", ErrTransportClosed)), true},
	}

	for _, tc := range tests {
		result := isExpectedShutdownErr(tc.err)
		assert.Equal(t, tc.expected, result, tc.name)
	}
}

func TestTransportClosedReturnsErrTransportClosed(t *testing.T) {
	t.Parallel()

	// Create a pair of connected Unix sockets
	socketPath := uniqueSocketPath(t, "closed")

	listener, listenErr := net.Listen("unix", socketPath)
	require.NoError(t, listenErr)
	defer listener.Close()

	var serverConn net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, _ = listener.Accept()
	}()

	clientConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)
	wg.Wait()
	require.NotNil(t, serverConn)
	defer serverConn.Close()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	transport := NewUnixTransportWithContext(ctx, clientConn)

	// Close the transport, then attempt to read
	closeErr := transport.Close()
	require.NoError(t, closeErr)

	_, readErr := transport.ReadMessage()
	require.Error(t, readErr)
	assert.ErrorIs(t, readErr, ErrTransportClosed, "ReadMessage after Close should return ErrTransportClosed")
	assert.True(t, isExpectedShutdownErr(readErr), "error from closed transport should be an expected shutdown error")

	writeErr := transport.WriteMessage(&dap.InitializeRequest{})
	require.Error(t, writeErr)
	assert.ErrorIs(t, writeErr, ErrTransportClosed, "WriteMessage after Close should return ErrTransportClosed")
	assert.True(t, isExpectedShutdownErr(writeErr), "error from closed transport should be an expected shutdown error")
}
