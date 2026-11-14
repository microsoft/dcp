/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package dap provides framed, language-neutral debug protocol mediation.
package dap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxMessageSize = 8 * 1024 * 1024

// Message preserves adapter-specific fields while exposing DAP correlation metadata.
type Message map[string]json.RawMessage

func (message Message) text(field string) string {
	var value string
	_ = json.Unmarshal(message[field], &value)
	return value
}

func (message Message) number(field string) int64 {
	var value int64
	_ = json.Unmarshal(message[field], &value)
	return value
}

func (message Message) setNumber(field string, value int64) {
	message[field] = json.RawMessage(strconv.FormatInt(value, 10))
}

// ReadMessage reads one Content-Length-framed message without interpreting extension payloads.
func ReadMessage(reader *bufio.Reader) (Message, error) {
	length := -1
	headerSize := 0
	for {
		line, readErr := reader.ReadSlice('\n')
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && (len(line) != 0 || headerSize != 0) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, readErr
		}
		headerSize += len(line)
		if headerSize > 4096 {
			return nil, fmt.Errorf("DAP headers exceed 4096 bytes")
		}
		if string(line) == "\r\n" {
			break
		}
		name, value, found := strings.Cut(strings.TrimSpace(string(line)), ":")
		if !found {
			return nil, fmt.Errorf("invalid DAP header")
		}
		if strings.EqualFold(name, "Content-Length") {
			if length >= 0 {
				return nil, fmt.Errorf("duplicate DAP content length")
			}
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil || parsed < 0 || parsed > maxMessageSize {
				return nil, fmt.Errorf("invalid DAP content length")
			}
			length = parsed
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing DAP content length")
	}
	payload := make([]byte, length)
	if _, readErr := io.ReadFull(reader, payload); readErr != nil {
		if errors.Is(readErr, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, readErr
	}
	var message Message
	if decodeErr := json.Unmarshal(payload, &message); decodeErr != nil {
		return nil, fmt.Errorf("decode DAP message: %w", decodeErr)
	}
	if message.number("seq") <= 0 {
		return nil, fmt.Errorf("invalid DAP sequence number")
	}
	switch message.text("type") {
	case "request":
		if message.text("command") == "" {
			return nil, fmt.Errorf("missing DAP command")
		}
	case "response":
		if message.number("request_seq") <= 0 {
			return nil, fmt.Errorf("invalid DAP response correlation")
		}
	case "event":
		if message.text("event") == "" {
			return nil, fmt.Errorf("missing DAP event")
		}
	default:
		return nil, fmt.Errorf("invalid DAP message type")
	}
	return message, nil
}

// WriteMessage writes a complete framed message; callers serialize concurrent writes.
func WriteMessage(writer io.Writer, message Message) error {
	payload, marshalErr := json.Marshal(message)
	if marshalErr != nil {
		return fmt.Errorf("encode DAP message: %w", marshalErr)
	}
	if len(payload) > maxMessageSize {
		return fmt.Errorf("DAP message exceeds size limit")
	}
	frame := append(fmt.Appendf(nil, "Content-Length: %d\r\n\r\n", len(payload)), payload...)
	written, writeErr := writer.Write(frame)
	if writeErr == nil && written != len(frame) {
		return io.ErrShortWrite
	}
	return writeErr
}
