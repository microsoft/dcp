/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/go-dap"
)

// newOutputEvent creates a DAP OutputEvent for sending text to the IDE.
// category should be "stdout", "stderr", or "console".
func newOutputEvent(seq int, category, output string) *dap.OutputEvent {
	return &dap.OutputEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  seq,
				Type: "event",
			},
			Event: "output",
		},
		Body: dap.OutputEventBody{
			Category: category,
			Output:   output,
		},
	}
}

// newTerminatedEvent creates a DAP TerminatedEvent to signal the debug session has ended.
func newTerminatedEvent(seq int) *dap.TerminatedEvent {
	return &dap.TerminatedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  seq,
				Type: "event",
			},
			Event: "terminated",
		},
	}
}

// RawMessage represents a DAP message that could not be decoded into a known type.
// This is used for custom/proprietary messages that the go-dap library doesn't recognize.
type RawMessage struct {
	// Data contains the raw JSON bytes of the message (without Content-Length header).
	Data []byte

	// header caches the parsed header to avoid repeated JSON unmarshaling.
	// It is invalidated (set to nil) when patchJSONFields modifies the raw data.
	header *rawMessageHeader
}

// rawMessageHeader contains the common fields present in all DAP protocol messages.
// It is used to extract header information from RawMessage instances.
type rawMessageHeader struct {
	Seq        int    `json:"seq"`
	Type       string `json:"type"`
	Command    string `json:"command,omitempty"`
	Event      string `json:"event,omitempty"`
	RequestSeq int    `json:"request_seq,omitempty"`
	Success    *bool  `json:"success,omitempty"`
	Message    string `json:"message,omitempty"`
}

// parseHeader parses the raw JSON into a rawMessageHeader, caching the result.
// Subsequent calls return the cached header without re-parsing.
// The cache is invalidated when patchJSONFields modifies the raw data.
func (r *RawMessage) parseHeader() rawMessageHeader {
	if r.header != nil {
		return *r.header
	}
	var h rawMessageHeader
	_ = json.Unmarshal(r.Data, &h)
	r.header = &h
	return h
}

// GetSeq extracts the sequence number from the raw message, or returns 0 if not parseable.
func (r *RawMessage) GetSeq() int {
	return r.parseHeader().Seq
}

// patchJSONFields patches multiple numeric JSON fields in the raw data in a single
// unmarshal/marshal pass. This invalidates the cached header.
func (r *RawMessage) patchJSONFields(fields map[string]int) error {
	if len(fields) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal(r.Data, &obj); unmarshalErr != nil {
		return fmt.Errorf("unmarshal raw message for patching: %w", unmarshalErr)
	}
	for field, value := range fields {
		valBytes, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return fmt.Errorf("marshal patch value for field %q: %w", field, marshalErr)
		}
		obj[field] = valBytes
	}
	patched, patchErr := json.Marshal(obj)
	if patchErr != nil {
		return fmt.Errorf("marshal patched raw message: %w", patchErr)
	}
	r.Data = patched
	r.header = nil // invalidate cache
	return nil
}

// ReadMessageWithFallback reads a DAP message from the given reader.
// If the message cannot be decoded (e.g., unknown command), it returns a RawMessage
// containing the raw bytes, allowing the message to be forwarded transparently.
func ReadMessageWithFallback(reader *bufio.Reader) (dap.Message, error) {
	content, readErr := dap.ReadBaseMessage(reader)
	if readErr != nil {
		return nil, readErr
	}

	msg, decodeErr := dap.DecodeProtocolMessage(content)
	if decodeErr != nil {
		// Check if this is an "unknown command/event" error from go-dap.
		// These errors indicate the message is valid DAP but uses a custom command.
		var fieldErr *dap.DecodeProtocolMessageFieldError
		if errors.As(decodeErr, &fieldErr) {
			// Return the raw message bytes so it can be forwarded transparently.
			return &RawMessage{Data: content}, nil
		}
		// Other decode errors (malformed JSON, etc.) should fail.
		return nil, decodeErr
	}

	return msg, nil
}

// WriteMessageWithFallback writes a DAP message to the given writer.
// If the message is a RawMessage, it writes the raw bytes directly.
// Otherwise, it uses the standard dap.WriteProtocolMessage.
func WriteMessageWithFallback(writer io.Writer, msg dap.Message) error {
	if raw, ok := msg.(*RawMessage); ok {
		return dap.WriteBaseMessage(writer, raw.Data)
	}
	return dap.WriteProtocolMessage(writer, msg)
}

// MessageEnvelope wraps a DAP message (typed or raw) and provides uniform access
// to common header fields. Header fields are extracted once at creation time and
// can be freely modified on the envelope. Changes are applied back to the underlying
// message in a single pass when Finalize is called, avoiding repeated
// serialization round trips.
type MessageEnvelope struct {
	// Inner is the underlying DAP message (typed or *RawMessage).
	Inner dap.Message

	// Seq is the message sequence number.
	Seq int

	// Type is the message type: "request", "response", or "event".
	Type string

	// Command is the command name (for requests and responses).
	Command string

	// Event is the event name (for events).
	Event string

	// RequestSeq is the sequence number of the corresponding request (for responses).
	RequestSeq int

	// Success indicates whether a response was successful (nil for non-responses).
	Success *bool

	// ErrorMessage is the error message for failed responses.
	ErrorMessage string

	// isRaw tracks whether Inner is a *RawMessage.
	isRaw bool

	// originalSeq and originalRequestSeq track the values at creation time
	// so Finalize only patches fields that actually changed.
	originalSeq        int
	originalRequestSeq int
}

// NewMessageEnvelope creates a MessageEnvelope by extracting header fields from the
// given message. For typed messages this is a zero-cost struct field read. For
// *RawMessage it performs a single JSON unmarshal of the header (which is cached
// on the RawMessage for any subsequent parseHeader calls).
func NewMessageEnvelope(msg dap.Message) *MessageEnvelope {
	env := &MessageEnvelope{Inner: msg}

	switch m := msg.(type) {
	case *RawMessage:
		env.isRaw = true
		h := m.parseHeader()
		env.Seq = h.Seq
		env.Type = h.Type
		env.Command = h.Command
		env.Event = h.Event
		env.RequestSeq = h.RequestSeq
		env.Success = h.Success
		env.ErrorMessage = h.Message
	case dap.RequestMessage:
		r := m.GetRequest()
		env.Seq = r.Seq
		env.Type = "request"
		env.Command = r.Command
	case dap.ResponseMessage:
		r := m.GetResponse()
		env.Seq = r.Seq
		env.Type = "response"
		env.Command = r.Command
		env.RequestSeq = r.RequestSeq
		env.Success = &r.Success
		env.ErrorMessage = r.Message
	case dap.EventMessage:
		e := m.GetEvent()
		env.Seq = e.Seq
		env.Type = "event"
		env.Event = e.Event
	default:
		env.Seq = msg.GetSeq()
	}

	env.originalSeq = env.Seq
	env.originalRequestSeq = env.RequestSeq
	return env
}

// GetSeq implements dap.Message.
func (e *MessageEnvelope) GetSeq() int {
	return e.Seq
}

// IsResponse returns true if the wrapped message is a response (typed or raw).
func (e *MessageEnvelope) IsResponse() bool {
	return e.Type == "response"
}

// Describe returns a human-readable description of the message for logging.
// It uses the pre-extracted header fields, so no additional parsing is required.
func (e *MessageEnvelope) Describe() string {
	prefix := ""
	if e.isRaw {
		prefix = "raw "
	}

	switch e.Type {
	case "request":
		return fmt.Sprintf("%srequest '%s' (seq=%d)", prefix, e.Command, e.Seq)
	case "response":
		success := e.Success != nil && *e.Success
		if success {
			return fmt.Sprintf("%sresponse '%s' (seq=%d, request_seq=%d, success=true)", prefix, e.Command, e.Seq, e.RequestSeq)
		}
		return fmt.Sprintf("%sresponse '%s' (seq=%d, request_seq=%d, success=false, message=%q)", prefix, e.Command, e.Seq, e.RequestSeq, e.ErrorMessage)
	case "event":
		return fmt.Sprintf("%sevent '%s' (seq=%d)", prefix, e.Event, e.Seq)
	default:
		if e.isRaw {
			return fmt.Sprintf("raw %s (seq=%d)", e.Type, e.Seq)
		}
		return fmt.Sprintf("unknown(seq=%d, type=%T)", e.Seq, e.Inner)
	}
}

// Finalize applies any modified header fields back to the underlying message and
// returns it, ready for writing to a Transport. For typed messages this is a
// zero-cost struct field write. For *RawMessage, changed fields are applied in
// a single patchJSONFields call (one unmarshal + one marshal). If no fields were
// changed, the raw data is left untouched.
func (e *MessageEnvelope) Finalize() (dap.Message, error) {
	if e.isRaw {
		raw := e.Inner.(*RawMessage)
		patches := make(map[string]int, 2)
		if e.Seq != e.originalSeq {
			patches["seq"] = e.Seq
		}
		if e.RequestSeq != e.originalRequestSeq {
			patches["request_seq"] = e.RequestSeq
		}
		if patchErr := raw.patchJSONFields(patches); patchErr != nil {
			return nil, fmt.Errorf("finalize raw message: %w", patchErr)
		}
		return raw, nil
	}

	// Typed messages: apply changes via struct field writes.
	switch m := e.Inner.(type) {
	case dap.RequestMessage:
		m.GetRequest().Seq = e.Seq
	case dap.ResponseMessage:
		r := m.GetResponse()
		r.Seq = e.Seq
		r.RequestSeq = e.RequestSeq
	case dap.EventMessage:
		m.GetEvent().Seq = e.Seq
	}

	return e.Inner, nil
}
