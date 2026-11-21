/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"bufio"
	"io"

	"github.com/gorilla/websocket"

	"github.com/microsoft/dcp/internal/dap"
)

type debugDAPTransport struct {
	reader *bufio.Reader
	writer io.Writer
	close  func() error
}

func (transport *debugDAPTransport) ReadMessage() (dap.Message, error) {
	return dap.ReadMessage(transport.reader)
}

func (transport *debugDAPTransport) WriteMessage(message dap.Message) error {
	return dap.WriteMessage(transport.writer, message)
}

func (transport *debugDAPTransport) Close() error {
	return transport.close()
}

type debugWebSocketStream struct {
	connection *websocket.Conn
	reader     io.Reader
}

func (stream *debugWebSocketStream) Read(payload []byte) (int, error) {
	for {
		if stream.reader == nil {
			_, reader, readErr := stream.connection.NextReader()
			if readErr != nil {
				return 0, readErr
			}
			stream.reader = reader
		}
		count, readErr := stream.reader.Read(payload)
		if readErr == io.EOF {
			stream.reader = nil
			if count == 0 {
				continue
			}
			readErr = nil
		}
		return count, readErr
	}
}

func (stream *debugWebSocketStream) Write(payload []byte) (int, error) {
	if writeErr := stream.connection.WriteMessage(websocket.BinaryMessage, payload); writeErr != nil {
		return 0, writeErr
	}
	return len(payload), nil
}
