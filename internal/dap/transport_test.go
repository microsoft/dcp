// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dap

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/go-dap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTCPTransport(t *testing.T) {
	t.Parallel()

	// Create a listener
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	defer listener.Close()

	// Accept connection in goroutine
	var serverConn net.Conn
	var acceptErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, acceptErr = listener.Accept()
	}()

	// Connect client
	clientConn, dialErr := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, dialErr)

	wg.Wait()
	require.NoError(t, acceptErr)
	require.NotNil(t, serverConn)

	defer clientConn.Close()
	defer serverConn.Close()

	clientTransport := NewTCPTransport(clientConn)
	serverTransport := NewTCPTransport(serverConn)

	t.Run("write and read message", func(t *testing.T) {
		// Client sends to server
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
	})

	t.Run("close prevents further operations", func(t *testing.T) {
		closeErr := clientTransport.Close()
		assert.NoError(t, closeErr)

		writeErr := clientTransport.WriteMessage(&dap.InitializeRequest{})
		assert.Error(t, writeErr)

		// Double close should be safe
		closeErr = clientTransport.Close()
		assert.NoError(t, closeErr)
	})
}

func TestDialTCP(t *testing.T) {
	t.Parallel()

	// Create a listener
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, listenErr)
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Accept in background
	go func() {
		conn, _ := listener.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	transport, dialErr := DialTCP(ctx, listener.Addr().String())
	require.NoError(t, dialErr)
	require.NotNil(t, transport)

	closeErr := transport.Close()
	assert.NoError(t, closeErr)
}

func TestDialTCP_InvalidAddress(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, dialErr := DialTCP(ctx, "127.0.0.1:0")
	assert.Error(t, dialErr)
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

func TestStdioTransport(t *testing.T) {
	t.Parallel()

	t.Run("write and read message", func(t *testing.T) {
		// Create connected pipes
		serverRead, clientWrite := io.Pipe()
		clientRead, serverWrite := io.Pipe()

		clientTransport := NewStdioTransport(clientRead, clientWrite)
		serverTransport := NewStdioTransport(serverRead, serverWrite)

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
	})

	t.Run("close prevents further operations", func(t *testing.T) {
		stdin := newMockReadWriteCloser()
		stdout := newMockReadWriteCloser()

		transport := NewStdioTransport(stdin, stdout)

		closeErr := transport.Close()
		assert.NoError(t, closeErr)

		writeErr := transport.WriteMessage(&dap.InitializeRequest{})
		assert.Error(t, writeErr)

		// Double close should be safe
		closeErr = transport.Close()
		assert.NoError(t, closeErr)
	})
}
