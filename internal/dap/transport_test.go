/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bytes"
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

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	clientTransport := NewTCPTransportWithContext(ctx, clientConn)
	serverTransport := NewTCPTransportWithContext(ctx, serverConn)

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

		// Double close should not panic
		_ = clientTransport.Close()
	})
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
	})

	t.Run("close prevents further operations", func(t *testing.T) {
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
	})
}

func TestUnixTransport(t *testing.T) {
	t.Parallel()

	// Create a temporary socket file with a short path (macOS has ~104 char limit for Unix socket paths)
	socketPath := uniqueSocketPath(t, "ut")

	// Create a listener
	listener, listenErr := net.Listen("unix", socketPath)
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
	clientConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)

	wg.Wait()
	require.NoError(t, acceptErr)
	require.NotNil(t, serverConn)

	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	clientTransport := NewUnixTransportWithContext(ctx, clientConn)
	serverTransport := NewUnixTransportWithContext(ctx, serverConn)

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

		// Double close should not panic
		_ = clientTransport.Close()
	})
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
