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

func TestHandshakeRequestResponse(t *testing.T) {
	t.Parallel()

	socketPath := uniqueSocketPath(t, "hs-rr")

	// Create server listener
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

	// Connect client
	clientConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)
	defer clientConn.Close()

	wg.Wait()
	require.NoError(t, acceptErr)
	defer serverConn.Close()

	t.Run("write and read request", func(t *testing.T) {
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
	})

	t.Run("write and read response", func(t *testing.T) {
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
	})

	t.Run("write and read error response", func(t *testing.T) {
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
	})
}

func TestHandshakeMessageSizeLimit(t *testing.T) {
	t.Parallel()

	socketPath := uniqueSocketPath(t, "hs-sz")

	listener, listenErr := net.Listen("unix", socketPath)
	require.NoError(t, listenErr)
	defer listener.Close()

	var wg sync.WaitGroup
	var serverConn net.Conn

	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, _ = listener.Accept()
	}()

	clientConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)
	defer clientConn.Close()

	wg.Wait()
	defer serverConn.Close()

	t.Run("rejects oversized message", func(t *testing.T) {
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
		err := writer.WriteRequest(req)
		assert.Error(t, err)
	})
}
