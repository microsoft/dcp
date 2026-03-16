/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// NOTE: Handshake validation (token/session verification) is handled directly
// by BridgeManager.validateHandshake, called from BridgeManager.handleConnection.
// No separate validator interface is needed since there is only one validation strategy.

// HandshakeRequest is sent by the IDE after connecting to the Unix socket.
// It contains authentication credentials, session identification, and debug adapter configuration.
type HandshakeRequest struct {
	// Token is the authentication token that must match the IDE session token.
	Token string `json:"token"`

	// SessionID identifies the debug session to connect to.
	SessionID string `json:"session_id"`

	// RunID is the IDE run session identifier.
	// This is used to correlate the debug bridge with the executable's output writers
	// so that debug adapter output can be captured to the correct log files.
	RunID string `json:"run_id,omitempty"`

	// DebugAdapterConfig contains the configuration for launching the debug adapter.
	// This is provided by the IDE during the handshake.
	DebugAdapterConfig *DebugAdapterConfig `json:"debug_adapter_config,omitempty"`
}

// HandshakeResponse is sent by the bridge after validating the handshake request.
type HandshakeResponse struct {
	// Success indicates whether the handshake was successful.
	Success bool `json:"success"`

	// Error contains the error message if Success is false.
	Error string `json:"error,omitempty"`
}

// ErrHandshakeFailed is returned when the handshake fails.
var ErrHandshakeFailed = errors.New("handshake failed")

// maxHandshakeMessageSize is the maximum size of a handshake message (64KB).
// This prevents denial-of-service attacks via large messages.
const maxHandshakeMessageSize = 64 * 1024

// HandshakeReader reads handshake messages from a connection.
// Messages are length-prefixed: 4-byte big-endian length followed by JSON payload.
type HandshakeReader struct {
	conn net.Conn
}

// NewHandshakeReader creates a new HandshakeReader for the given connection.
func NewHandshakeReader(conn net.Conn) *HandshakeReader {
	return &HandshakeReader{conn: conn}
}

// ReadRequest reads a HandshakeRequest from the connection.
func (r *HandshakeReader) ReadRequest() (*HandshakeRequest, error) {
	data, readErr := r.readMessage()
	if readErr != nil {
		return nil, fmt.Errorf("failed to read handshake request: %w", readErr)
	}

	var req HandshakeRequest
	if unmarshalErr := json.Unmarshal(data, &req); unmarshalErr != nil {
		return nil, fmt.Errorf("failed to unmarshal handshake request: %w", unmarshalErr)
	}

	return &req, nil
}

// readMessage reads a length-prefixed message from the connection.
func (r *HandshakeReader) readMessage() ([]byte, error) {
	// Read 4-byte length prefix (big-endian)
	var lengthBuf [4]byte
	if _, readErr := io.ReadFull(r.conn, lengthBuf[:]); readErr != nil {
		return nil, fmt.Errorf("failed to read message length: %w", readErr)
	}

	length := binary.BigEndian.Uint32(lengthBuf[:])
	if length == 0 {
		return nil, errors.New("message length is zero")
	}
	if length > maxHandshakeMessageSize {
		return nil, fmt.Errorf("message length %d exceeds maximum %d", length, maxHandshakeMessageSize)
	}

	// Read the message body
	data := make([]byte, length)
	if _, readErr := io.ReadFull(r.conn, data); readErr != nil {
		return nil, fmt.Errorf("failed to read message body: %w", readErr)
	}

	return data, nil
}

// HandshakeWriter writes handshake messages to a connection.
// Messages are length-prefixed: 4-byte big-endian length followed by JSON payload.
type HandshakeWriter struct {
	conn net.Conn
}

// NewHandshakeWriter creates a new HandshakeWriter for the given connection.
func NewHandshakeWriter(conn net.Conn) *HandshakeWriter {
	return &HandshakeWriter{conn: conn}
}

// WriteResponse writes a HandshakeResponse to the connection.
func (w *HandshakeWriter) WriteResponse(resp *HandshakeResponse) error {
	data, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal handshake response: %w", marshalErr)
	}

	return w.writeMessage(data)
}

// WriteRequest writes a HandshakeRequest to the connection.
// This is used by the client side (IDE) to initiate the handshake.
func (w *HandshakeWriter) WriteRequest(req *HandshakeRequest) error {
	data, marshalErr := json.Marshal(req)
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal handshake request: %w", marshalErr)
	}

	return w.writeMessage(data)
}

// writeMessage writes a length-prefixed message to the connection.
func (w *HandshakeWriter) writeMessage(data []byte) error {
	if len(data) > maxHandshakeMessageSize {
		return fmt.Errorf("message length %d exceeds maximum %d", len(data), maxHandshakeMessageSize)
	}

	// Write 4-byte length prefix (big-endian)
	var lengthBuf [4]byte
	binary.BigEndian.PutUint32(lengthBuf[:], uint32(len(data)))

	if _, writeErr := w.conn.Write(lengthBuf[:]); writeErr != nil {
		return fmt.Errorf("failed to write message length: %w", writeErr)
	}

	// Write the message body
	if _, writeErr := w.conn.Write(data); writeErr != nil {
		return fmt.Errorf("failed to write message body: %w", writeErr)
	}

	return nil
}

// ReadResponse reads a HandshakeResponse from the connection.
// This is used by the client side (IDE) to receive the handshake result.
func (r *HandshakeReader) ReadResponse() (*HandshakeResponse, error) {
	data, readErr := r.readMessage()
	if readErr != nil {
		return nil, fmt.Errorf("failed to read handshake response: %w", readErr)
	}

	var resp HandshakeResponse
	if unmarshalErr := json.Unmarshal(data, &resp); unmarshalErr != nil {
		return nil, fmt.Errorf("failed to unmarshal handshake response: %w", unmarshalErr)
	}

	return &resp, nil
}

// performClientHandshake sends a handshake request and waits for the response.
// This is a convenience function for the client side (IDE).
// Returns nil on success, or an error on failure.
func performClientHandshake(conn net.Conn, token, sessionID, runID string) error {
	writer := NewHandshakeWriter(conn)
	reader := NewHandshakeReader(conn)

	// Send the handshake request
	req := &HandshakeRequest{
		Token:     token,
		SessionID: sessionID,
		RunID:     runID,
	}
	if writeErr := writer.WriteRequest(req); writeErr != nil {
		return fmt.Errorf("failed to send handshake request: %w", writeErr)
	}

	// Read the response
	resp, readErr := reader.ReadResponse()
	if readErr != nil {
		return fmt.Errorf("failed to read handshake response: %w", readErr)
	}

	if !resp.Success {
		if resp.Error != "" {
			return fmt.Errorf("%w: %s", ErrHandshakeFailed, resp.Error)
		}
		return ErrHandshakeFailed
	}

	return nil
}
