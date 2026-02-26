/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupHandshakeConn creates a Unix socket pair for handshake testing.
func setupHandshakeConn(t *testing.T, suffix string) (net.Conn, net.Conn) {
	t.Helper()

	socketPath := uniqueSocketPath(t, suffix)

	listener, listenErr := net.Listen("unix", socketPath)
	require.NoError(t, listenErr)
	defer listener.Close()

	var wg sync.WaitGroup
	var serverConn net.Conn
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

	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})

	return clientConn, serverConn
}

func TestHandshakeWriteAndReadRequest(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := setupHandshakeConn(t, "hs-rq")

	clientWriter := NewHandshakeWriter(clientConn)
	serverReader := NewHandshakeReader(serverConn)

	req := &HandshakeRequest{
		Token:     "test-token-123",
		SessionID: "session-456",
	}

	writeErr := clientWriter.WriteRequest(req)
	require.NoError(t, writeErr)

	receivedReq, readErr := serverReader.ReadRequest()
	require.NoError(t, readErr)

	assert.Equal(t, req.Token, receivedReq.Token)
	assert.Equal(t, req.SessionID, receivedReq.SessionID)
}

func TestHandshakeWriteAndReadResponse(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := setupHandshakeConn(t, "hs-rs")

	serverWriter := NewHandshakeWriter(serverConn)
	clientReader := NewHandshakeReader(clientConn)

	resp := &HandshakeResponse{
		Success: true,
	}

	writeErr := serverWriter.WriteResponse(resp)
	require.NoError(t, writeErr)

	receivedResp, readErr := clientReader.ReadResponse()
	require.NoError(t, readErr)

	assert.True(t, receivedResp.Success)
	assert.Empty(t, receivedResp.Error)
}

func TestHandshakeWriteAndReadErrorResponse(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := setupHandshakeConn(t, "hs-er")

	serverWriter := NewHandshakeWriter(serverConn)
	clientReader := NewHandshakeReader(clientConn)

	resp := &HandshakeResponse{
		Success: false,
		Error:   "authentication failed",
	}

	writeErr := serverWriter.WriteResponse(resp)
	require.NoError(t, writeErr)

	receivedResp, readErr := clientReader.ReadResponse()
	require.NoError(t, readErr)

	assert.False(t, receivedResp.Success)
	assert.Equal(t, "authentication failed", receivedResp.Error)
}

func TestHandshakeRejectsOversizedMessage(t *testing.T) {
	t.Parallel()

	clientConn, _ := setupHandshakeConn(t, "hs-sz")

	writer := NewHandshakeWriter(clientConn)

	// Create a request with a very long token
	largeToken := make([]byte, maxHandshakeMessageSize+1)
	for i := range largeToken {
		largeToken[i] = 'a'
	}

	req := &HandshakeRequest{
		Token:     string(largeToken),
		SessionID: "session",
	}

	// Writing should fail due to size limit
	writeErr := writer.WriteRequest(req)
	assert.Error(t, writeErr)
}
