/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/google/go-dap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadMessageWithFallback(t *testing.T) {
	t.Parallel()

	t.Run("known request is decoded normally", func(t *testing.T) {
		t.Parallel()

		// Create a valid DAP message using WriteProtocolMessage
		buf := new(bytes.Buffer)
		initReq := &dap.InitializeRequest{
			Request: dap.Request{
				ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
				Command:         "initialize",
			},
		}
		err := dap.WriteProtocolMessage(buf, initReq)
		require.NoError(t, err)

		reader := bufio.NewReader(buf)
		msg, readErr := ReadMessageWithFallback(reader)
		require.NoError(t, readErr)

		decoded, ok := msg.(*dap.InitializeRequest)
		require.True(t, ok, "expected *dap.InitializeRequest, got %T", msg)
		assert.Equal(t, 1, decoded.Seq)
		assert.Equal(t, "initialize", decoded.Command)
	})

	t.Run("unknown request returns RawMessage", func(t *testing.T) {
		t.Parallel()

		// Create a DAP message with unknown command
		customJSON := `{"seq":2,"type":"request","command":"handshake","arguments":{"value":"test-value"}}`
		content := "Content-Length: " + itoa(len(customJSON)) + "\r\n\r\n" + customJSON

		reader := bufio.NewReader(bytes.NewBufferString(content))
		msg, readErr := ReadMessageWithFallback(reader)
		require.NoError(t, readErr)

		raw, ok := msg.(*RawMessage)
		require.True(t, ok, "expected *RawMessage, got %T", msg)
		assert.Equal(t, 2, raw.GetSeq())
		assert.Contains(t, string(raw.Data), `"command":"handshake"`)
	})

	t.Run("unknown event returns RawMessage", func(t *testing.T) {
		t.Parallel()

		customJSON := `{"seq":5,"type":"event","event":"customEvent","body":{"data":123}}`
		content := "Content-Length: " + itoa(len(customJSON)) + "\r\n\r\n" + customJSON

		reader := bufio.NewReader(bytes.NewBufferString(content))
		msg, readErr := ReadMessageWithFallback(reader)
		require.NoError(t, readErr)

		raw, ok := msg.(*RawMessage)
		require.True(t, ok, "expected *RawMessage, got %T", msg)
		assert.Equal(t, 5, raw.GetSeq())
		assert.Contains(t, string(raw.Data), `"event":"customEvent"`)
	})

	t.Run("malformed JSON returns error", func(t *testing.T) {
		t.Parallel()

		badJSON := `{"seq":1,"type":`
		content := "Content-Length: " + itoa(len(badJSON)) + "\r\n\r\n" + badJSON

		reader := bufio.NewReader(bytes.NewBufferString(content))
		_, readErr := ReadMessageWithFallback(reader)
		require.Error(t, readErr)
	})
}

func TestWriteMessageWithFallback(t *testing.T) {
	t.Parallel()

	t.Run("known message uses standard encoding", func(t *testing.T) {
		t.Parallel()

		buf := new(bytes.Buffer)
		initReq := &dap.InitializeRequest{
			Request: dap.Request{
				ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
				Command:         "initialize",
			},
		}
		err := WriteMessageWithFallback(buf, initReq)
		require.NoError(t, err)

		// Read it back
		reader := bufio.NewReader(buf)
		msg, readErr := dap.ReadProtocolMessage(reader)
		require.NoError(t, readErr)

		decoded, ok := msg.(*dap.InitializeRequest)
		require.True(t, ok)
		assert.Equal(t, 1, decoded.Seq)
	})

	t.Run("RawMessage writes raw bytes", func(t *testing.T) {
		t.Parallel()

		customJSON := `{"seq":2,"type":"request","command":"handshake","arguments":{"value":"test-value"}}`
		raw := &RawMessage{Data: []byte(customJSON)}

		buf := new(bytes.Buffer)
		err := WriteMessageWithFallback(buf, raw)
		require.NoError(t, err)

		// Expect Content-Length header followed by the raw JSON
		result := buf.String()
		assert.Contains(t, result, "Content-Length:")
		assert.Contains(t, result, customJSON)
	})

	t.Run("RawMessage roundtrip preserves data", func(t *testing.T) {
		t.Parallel()

		originalJSON := `{"seq":3,"type":"request","command":"vsdbgHandshake","arguments":{"protocolVersion":1}}`
		raw := &RawMessage{Data: []byte(originalJSON)}

		buf := new(bytes.Buffer)
		err := WriteMessageWithFallback(buf, raw)
		require.NoError(t, err)

		// Read it back using ReadMessageWithFallback
		reader := bufio.NewReader(buf)
		msg, readErr := ReadMessageWithFallback(reader)
		require.NoError(t, readErr)

		readRaw, ok := msg.(*RawMessage)
		require.True(t, ok, "expected *RawMessage, got %T", msg)
		assert.Equal(t, originalJSON, string(readRaw.Data))
	})
}

// itoa is a simple helper to convert int to string without importing strconv
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestMessageEnvelope_TypedRequest(t *testing.T) {
	t.Parallel()

	msg := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "initialize",
		},
	}

	env := NewMessageEnvelope(msg)
	assert.Equal(t, 1, env.Seq)
	assert.Equal(t, "request", env.Type)
	assert.Equal(t, "initialize", env.Command)
	assert.False(t, env.IsResponse())

	// Modify seq
	env.Seq = 100
	finalized, finalizeErr := env.Finalize()
	require.NoError(t, finalizeErr)
	assert.Equal(t, 100, finalized.GetSeq())
	assert.Equal(t, msg, finalized) // same pointer
}

func TestMessageEnvelope_TypedResponse(t *testing.T) {
	t.Parallel()

	msg := &dap.InitializeResponse{
		Response: dap.Response{
			ProtocolMessage: dap.ProtocolMessage{Seq: 2, Type: "response"},
			Command:         "initialize",
			RequestSeq:      1,
			Success:         true,
		},
	}

	env := NewMessageEnvelope(msg)
	assert.Equal(t, 2, env.Seq)
	assert.Equal(t, "response", env.Type)
	assert.Equal(t, 1, env.RequestSeq)
	assert.True(t, env.IsResponse())
	require.NotNil(t, env.Success)
	assert.True(t, *env.Success)

	// Modify both seq and request_seq
	env.Seq = 200
	env.RequestSeq = 50
	finalized, finalizeErr := env.Finalize()
	require.NoError(t, finalizeErr)
	assert.Equal(t, 200, finalized.GetSeq())
	resp := finalized.(*dap.InitializeResponse)
	assert.Equal(t, 50, resp.Response.RequestSeq)
}

func TestMessageEnvelope_TypedEvent(t *testing.T) {
	t.Parallel()

	msg := &dap.OutputEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{Seq: 3, Type: "event"},
			Event:           "output",
		},
	}

	env := NewMessageEnvelope(msg)
	assert.Equal(t, 3, env.Seq)
	assert.Equal(t, "event", env.Type)
	assert.Equal(t, "output", env.Event)
	assert.False(t, env.IsResponse())

	// Modify seq
	env.Seq = 300
	finalized, finalizeErr := env.Finalize()
	require.NoError(t, finalizeErr)
	assert.Equal(t, 300, finalized.GetSeq())
}

func TestMessageEnvelope_RawRequest(t *testing.T) {
	t.Parallel()

	raw := &RawMessage{Data: []byte(`{"seq":5,"type":"request","command":"handshake","arguments":{"v":1}}`)}
	env := NewMessageEnvelope(raw)

	assert.Equal(t, 5, env.Seq)
	assert.Equal(t, "request", env.Type)
	assert.Equal(t, "handshake", env.Command)
	assert.False(t, env.IsResponse())

	// Modify seq
	env.Seq = 500
	finalized, finalizeErr := env.Finalize()
	require.NoError(t, finalizeErr)

	// Finalize returns the same RawMessage with patched JSON
	patchedRaw, ok := finalized.(*RawMessage)
	require.True(t, ok)
	assert.Equal(t, 500, patchedRaw.GetSeq())
	assert.Contains(t, string(patchedRaw.Data), `"command":"handshake"`)
	assert.Contains(t, string(patchedRaw.Data), `"arguments"`)
}

func TestMessageEnvelope_RawResponse(t *testing.T) {
	t.Parallel()

	raw := &RawMessage{Data: []byte(`{"seq":6,"type":"response","command":"handshake","request_seq":5,"success":true,"body":{"v":1}}`)}
	env := NewMessageEnvelope(raw)

	assert.Equal(t, 6, env.Seq)
	assert.Equal(t, "response", env.Type)
	assert.Equal(t, 5, env.RequestSeq)
	assert.True(t, env.IsResponse())
	require.NotNil(t, env.Success)
	assert.True(t, *env.Success)

	// Modify both seq and request_seq — should produce a single patch pass
	env.Seq = 100
	env.RequestSeq = 42
	finalized, finalizeErr := env.Finalize()
	require.NoError(t, finalizeErr)

	patchedRaw, ok := finalized.(*RawMessage)
	require.True(t, ok)
	assert.Equal(t, 100, patchedRaw.GetSeq())
	h := patchedRaw.parseHeader()
	assert.Equal(t, 42, h.RequestSeq)
	assert.Equal(t, "handshake", h.Command)
	assert.Contains(t, string(patchedRaw.Data), `"body"`)
}

func TestMessageEnvelope_NoChanges(t *testing.T) {
	t.Parallel()

	originalJSON := `{"seq":3,"type":"event","event":"custom","body":{"data":123}}`
	raw := &RawMessage{Data: []byte(originalJSON)}
	env := NewMessageEnvelope(raw)

	// Don't modify anything
	finalized, finalizeErr := env.Finalize()
	require.NoError(t, finalizeErr)

	patchedRaw, ok := finalized.(*RawMessage)
	require.True(t, ok)
	// Data should be untouched since nothing changed
	assert.Equal(t, originalJSON, string(patchedRaw.Data))
}

func TestMessageEnvelope_Describe(t *testing.T) {
	t.Parallel()

	t.Run("typed request", func(t *testing.T) {
		t.Parallel()
		msg := &dap.InitializeRequest{
			Request: dap.Request{
				ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
				Command:         "initialize",
			},
		}
		env := NewMessageEnvelope(msg)
		assert.Equal(t, "request 'initialize' (seq=1)", env.Describe())
	})

	t.Run("typed response success", func(t *testing.T) {
		t.Parallel()
		msg := &dap.InitializeResponse{
			Response: dap.Response{
				ProtocolMessage: dap.ProtocolMessage{Seq: 2, Type: "response"},
				Command:         "initialize",
				RequestSeq:      1,
				Success:         true,
			},
		}
		env := NewMessageEnvelope(msg)
		assert.Equal(t, "response 'initialize' (seq=2, request_seq=1, success=true)", env.Describe())
	})

	t.Run("raw request", func(t *testing.T) {
		t.Parallel()
		msg := &RawMessage{Data: []byte(`{"seq":5,"type":"request","command":"vsdbgHandshake"}`)}
		env := NewMessageEnvelope(msg)
		assert.Equal(t, "raw request 'vsdbgHandshake' (seq=5)", env.Describe())
	})

	t.Run("raw response success", func(t *testing.T) {
		t.Parallel()
		msg := &RawMessage{Data: []byte(`{"seq":6,"type":"response","command":"vsdbgHandshake","request_seq":5,"success":true}`)}
		env := NewMessageEnvelope(msg)
		assert.Equal(t, "raw response 'vsdbgHandshake' (seq=6, request_seq=5, success=true)", env.Describe())
	})

	t.Run("raw response failure", func(t *testing.T) {
		t.Parallel()
		msg := &RawMessage{Data: []byte(`{"seq":7,"type":"response","command":"vsdbgHandshake","request_seq":5,"success":false,"message":"denied"}`)}
		env := NewMessageEnvelope(msg)
		assert.Equal(t, "raw response 'vsdbgHandshake' (seq=7, request_seq=5, success=false, message=\"denied\")", env.Describe())
	})

	t.Run("raw event", func(t *testing.T) {
		t.Parallel()
		msg := &RawMessage{Data: []byte(`{"seq":8,"type":"event","event":"customNotify"}`)}
		env := NewMessageEnvelope(msg)
		assert.Equal(t, "raw event 'customNotify' (seq=8)", env.Describe())
	})

	t.Run("raw unknown type", func(t *testing.T) {
		t.Parallel()
		msg := &RawMessage{Data: []byte(`{"seq":9,"type":"weird"}`)}
		env := NewMessageEnvelope(msg)
		assert.Equal(t, "raw weird (seq=9)", env.Describe())
	})

	t.Run("describe reflects modified seq", func(t *testing.T) {
		t.Parallel()
		msg := &RawMessage{Data: []byte(`{"seq":5,"type":"request","command":"handshake"}`)}
		env := NewMessageEnvelope(msg)
		env.Seq = 99
		assert.Equal(t, "raw request 'handshake' (seq=99)", env.Describe())
	})
}

func TestPatchJSONFields(t *testing.T) {
	t.Parallel()

	t.Run("single field", func(t *testing.T) {
		t.Parallel()
		raw := &RawMessage{Data: []byte(`{"seq":1,"type":"request","command":"test"}`)}
		require.NoError(t, raw.patchJSONFields(map[string]int{"seq": 42}))
		assert.Equal(t, 42, raw.GetSeq())
		assert.Contains(t, string(raw.Data), `"command":"test"`)
	})

	t.Run("multiple fields", func(t *testing.T) {
		t.Parallel()
		raw := &RawMessage{Data: []byte(`{"seq":1,"type":"response","command":"test","request_seq":5,"success":true}`)}
		require.NoError(t, raw.patchJSONFields(map[string]int{"seq": 100, "request_seq": 42}))
		h := raw.parseHeader()
		assert.Equal(t, 100, h.Seq)
		assert.Equal(t, 42, h.RequestSeq)
		assert.Equal(t, "test", h.Command)
		require.NotNil(t, h.Success)
		assert.True(t, *h.Success)
	})

	t.Run("empty fields is no-op", func(t *testing.T) {
		t.Parallel()
		original := `{"seq":1,"type":"request"}`
		raw := &RawMessage{Data: []byte(original)}
		require.NoError(t, raw.patchJSONFields(map[string]int{}))
		assert.Equal(t, original, string(raw.Data))
	})

	t.Run("preserves body", func(t *testing.T) {
		t.Parallel()
		raw := &RawMessage{Data: []byte(`{"seq":1,"type":"response","command":"test","request_seq":3,"success":true,"body":{"value":"test"}}`)}
		require.NoError(t, raw.patchJSONFields(map[string]int{"seq": 42}))
		assert.Contains(t, string(raw.Data), `"body"`)
		assert.Contains(t, string(raw.Data), `"value":"test"`)
	})

	t.Run("invalidates header cache", func(t *testing.T) {
		t.Parallel()
		raw := &RawMessage{Data: []byte(`{"seq":1,"type":"request","command":"test"}`)}
		// Populate cache
		h1 := raw.parseHeader()
		assert.Equal(t, 1, h1.Seq)
		assert.NotNil(t, raw.header)
		// Patch
		require.NoError(t, raw.patchJSONFields(map[string]int{"seq": 99}))
		// Cache should be invalidated
		assert.Nil(t, raw.header)
		// Re-parse should reflect new value
		h2 := raw.parseHeader()
		assert.Equal(t, 99, h2.Seq)
	})
}
