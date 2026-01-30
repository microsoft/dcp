/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"testing"
	"time"

	"github.com/google/go-dap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSequenceCounter(t *testing.T) {
	t.Parallel()

	counter := newSequenceCounter()

	assert.Equal(t, 0, counter.Current(), "initial value should be 0")

	assert.Equal(t, 1, counter.Next(), "first Next() should return 1")
	assert.Equal(t, 1, counter.Current(), "Current() should return 1 after first Next()")

	assert.Equal(t, 2, counter.Next(), "second Next() should return 2")
	assert.Equal(t, 3, counter.Next(), "third Next() should return 3")
	assert.Equal(t, 3, counter.Current(), "Current() should return 3")
}

func TestPendingRequestMap(t *testing.T) {
	t.Parallel()

	m := newPendingRequestMap()

	assert.Equal(t, 0, m.Len(), "initial map should be empty")

	// Add requests
	req1 := &pendingRequest{
		originalSeq: 1,
		virtual:     false,
		request:     &dap.ContinueRequest{},
	}
	req2 := &pendingRequest{
		originalSeq:  0,
		virtual:      true,
		responseChan: make(chan dap.Message, 1),
		request:      &dap.ThreadsRequest{},
	}

	m.Add(10, req1)
	m.Add(11, req2)

	assert.Equal(t, 2, m.Len(), "map should have 2 entries")

	// Get request
	got := m.Get(10)
	require.NotNil(t, got, "should get request for seq 10")
	assert.Equal(t, req1, got)
	assert.Equal(t, 1, m.Len(), "map should have 1 entry after Get")

	// Get same request again should return nil
	got = m.Get(10)
	assert.Nil(t, got, "second Get for same seq should return nil")

	// Get unknown request
	got = m.Get(999)
	assert.Nil(t, got, "Get for unknown seq should return nil")

	// Get remaining request
	got = m.Get(11)
	require.NotNil(t, got, "should get request for seq 11")
	assert.Equal(t, req2, got)
	assert.Equal(t, 0, m.Len(), "map should be empty")
}

func TestPendingRequestMap_DrainWithError(t *testing.T) {
	t.Parallel()

	m := newPendingRequestMap()

	// Add virtual request with response channel
	responseChan := make(chan dap.Message, 1)
	m.Add(10, &pendingRequest{
		virtual:      true,
		responseChan: responseChan,
	})

	// Add non-virtual request
	m.Add(11, &pendingRequest{
		virtual: false,
	})

	assert.Equal(t, 2, m.Len())

	// Drain
	m.DrainWithError()

	assert.Equal(t, 0, m.Len(), "map should be empty after drain")

	// Response channel should be closed
	select {
	case _, ok := <-responseChan:
		assert.False(t, ok, "response channel should be closed")
	default:
		t.Fatal("response channel should be closed and readable")
	}
}

func TestDirection_String(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "upstream", Upstream.String())
	assert.Equal(t, "downstream", Downstream.String())
	assert.Equal(t, "unknown", Direction(99).String())
}

func TestComposeHandlers(t *testing.T) {
	t.Parallel()

	callOrder := []string{}

	h1 := func(msg dap.Message, dir Direction) (dap.Message, bool) {
		callOrder = append(callOrder, "h1")
		return msg, true
	}

	h2 := func(msg dap.Message, dir Direction) (dap.Message, bool) {
		callOrder = append(callOrder, "h2")
		return msg, true
	}

	composed := ComposeHandlers(h1, h2)
	msg := &dap.InitializeRequest{}

	_, forward := composed(msg, Upstream)

	assert.True(t, forward)
	assert.Equal(t, []string{"h1", "h2"}, callOrder)
}

func TestComposeHandlers_StopsOnForwardFalse(t *testing.T) {
	t.Parallel()

	callOrder := []string{}

	h1 := func(msg dap.Message, dir Direction) (dap.Message, bool) {
		callOrder = append(callOrder, "h1")
		return nil, false // Stop forwarding
	}

	h2 := func(msg dap.Message, dir Direction) (dap.Message, bool) {
		callOrder = append(callOrder, "h2")
		return msg, true
	}

	composed := ComposeHandlers(h1, h2)
	msg := &dap.InitializeRequest{}

	_, forward := composed(msg, Upstream)

	assert.False(t, forward)
	assert.Equal(t, []string{"h1"}, callOrder, "h2 should not be called")
}

func TestComposeHandlers_PassesModifiedMessage(t *testing.T) {
	t.Parallel()

	h1 := func(msg dap.Message, dir Direction) (dap.Message, bool) {
		// Modify the message
		return &dap.ContinueRequest{}, true
	}

	h2 := func(msg dap.Message, dir Direction) (dap.Message, bool) {
		// Check that we received the modified message
		_, ok := msg.(*dap.ContinueRequest)
		assert.True(t, ok, "h2 should receive modified message")
		return msg, true
	}

	composed := ComposeHandlers(h1, h2)
	msg := &dap.InitializeRequest{}

	result, forward := composed(msg, Upstream)

	assert.True(t, forward)
	_, ok := result.(*dap.ContinueRequest)
	assert.True(t, ok, "result should be modified message")
}

func TestInitializeRequestHandler(t *testing.T) {
	t.Parallel()

	handler := initializeRequestHandler()

	t.Run("modifies upstream InitializeRequest", func(t *testing.T) {
		req := &dap.InitializeRequest{
			Request: dap.Request{
				ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
				Command:         "initialize",
			},
			Arguments: dap.InitializeRequestArguments{
				SupportsRunInTerminalRequest: false,
			},
		}

		modified, forward := handler(req, Upstream)

		assert.True(t, forward)
		initReq, ok := modified.(*dap.InitializeRequest)
		require.True(t, ok)
		assert.True(t, initReq.Arguments.SupportsRunInTerminalRequest)
	})

	t.Run("does not modify downstream InitializeRequest", func(t *testing.T) {
		req := &dap.InitializeRequest{
			Arguments: dap.InitializeRequestArguments{
				SupportsRunInTerminalRequest: false,
			},
		}

		modified, forward := handler(req, Downstream)

		assert.True(t, forward)
		initReq, ok := modified.(*dap.InitializeRequest)
		require.True(t, ok)
		assert.False(t, initReq.Arguments.SupportsRunInTerminalRequest, "downstream should not be modified")
	})

	t.Run("passes through other messages", func(t *testing.T) {
		req := &dap.ContinueRequest{}

		modified, forward := handler(req, Upstream)

		assert.True(t, forward)
		assert.Equal(t, req, modified)
	})
}

func TestEventDeduplicator(t *testing.T) {
	t.Parallel()

	t.Run("suppresses duplicate event within window", func(t *testing.T) {
		d := newEventDeduplicator(100 * time.Millisecond)

		event := &dap.ContinuedEvent{
			Event: dap.Event{
				ProtocolMessage: dap.ProtocolMessage{Type: "event"},
				Event:           "continued",
			},
			Body: dap.ContinuedEventBody{
				ThreadId: 1,
			},
		}

		// Record virtual event
		d.RecordVirtualEvent(event)

		// Same event should be suppressed
		assert.True(t, d.ShouldSuppress(event))

		// Second suppression should not suppress (entry was removed)
		assert.False(t, d.ShouldSuppress(event))
	})

	t.Run("does not suppress after window expires", func(t *testing.T) {
		now := time.Now()
		d := newEventDeduplicator(100 * time.Millisecond)
		d.timeSource = func() time.Time { return now }

		event := &dap.ContinuedEvent{
			Body: dap.ContinuedEventBody{ThreadId: 1},
		}

		d.RecordVirtualEvent(event)

		// Advance time past window
		d.timeSource = func() time.Time { return now.Add(150 * time.Millisecond) }

		assert.False(t, d.ShouldSuppress(event))
	})

	t.Run("does not suppress different events", func(t *testing.T) {
		d := newEventDeduplicator(100 * time.Millisecond)

		event1 := &dap.ContinuedEvent{
			Body: dap.ContinuedEventBody{ThreadId: 1},
		}
		event2 := &dap.ContinuedEvent{
			Body: dap.ContinuedEventBody{ThreadId: 2},
		}

		d.RecordVirtualEvent(event1)

		// Different thread ID should not be suppressed
		assert.False(t, d.ShouldSuppress(event2))
	})

	t.Run("does not suppress output events", func(t *testing.T) {
		d := newEventDeduplicator(100 * time.Millisecond)

		event := &dap.OutputEvent{
			Body: dap.OutputEventBody{
				Output:   "test output",
				Category: "console",
			},
		}

		d.RecordVirtualEvent(event)
		assert.False(t, d.ShouldSuppress(event), "output events should not be deduplicated")
	})
}

func TestDefaultTerminalHandler(t *testing.T) {
	t.Parallel()

	handler := defaultTerminalHandler()

	req := &dap.RunInTerminalRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 5, Type: "request"},
			Command:         "runInTerminal",
		},
		Arguments: dap.RunInTerminalRequestArguments{
			Kind:  "integrated",
			Title: "Test",
			Cwd:   "/tmp",
			Args:  []string{"echo", "hello"},
		},
	}

	response := handler(req)

	assert.Equal(t, "response", response.Type)
	assert.Equal(t, "runInTerminal", response.Command)
	assert.Equal(t, 5, response.RequestSeq)
	assert.True(t, response.Success)
}
