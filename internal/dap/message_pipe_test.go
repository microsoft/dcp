/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-dap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

func TestMessagePipe_FIFOOrderAndMonotonicSeq(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	transport := NewUnixTransportWithContext(ctx, serverConn)
	pipe := NewMessagePipe(ctx, transport, "test", logr.Discard())

	// Start the writer goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- pipe.Run(ctx)
	}()

	// Enqueue several messages with arbitrary original seq values.
	messageCount := 10
	for i := 0; i < messageCount; i++ {
		env := NewMessageEnvelope(&dap.SetBreakpointsRequest{
			Request: dap.Request{
				ProtocolMessage: dap.ProtocolMessage{Seq: 100 + i, Type: "request"},
				Command:         "setBreakpoints",
			},
		})
		pipe.Send(env)
	}

	// Read messages from the client side and verify ordering.
	clientTransport := NewUnixTransportWithContext(ctx, clientConn)
	for i := 0; i < messageCount; i++ {
		msg, readErr := clientTransport.ReadMessage()
		require.NoError(t, readErr)
		assert.Equal(t, i+1, msg.GetSeq(), "seq should be monotonically increasing starting at 1")
	}

	cancel()
	<-errCh
}

func TestMessagePipe_ConcurrentSendAllWritten(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	transport := NewUnixTransportWithContext(ctx, serverConn)
	pipe := NewMessagePipe(ctx, transport, "test", logr.Discard())

	errCh := make(chan error, 1)
	go func() {
		errCh <- pipe.Run(ctx)
	}()

	// Send messages from multiple goroutines concurrently.
	goroutineCount := 5
	messagesPerGoroutine := 10
	totalMessages := goroutineCount * messagesPerGoroutine

	var wg sync.WaitGroup
	wg.Add(goroutineCount)
	for g := 0; g < goroutineCount; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < messagesPerGoroutine; i++ {
				env := NewMessageEnvelope(&dap.ContinueRequest{
					Request: dap.Request{
						ProtocolMessage: dap.ProtocolMessage{Seq: 999, Type: "request"},
						Command:         "continue",
					},
				})
				pipe.Send(env)
			}
		}()
	}
	wg.Wait()

	// Read all messages and verify we got the right count and monotonic seq.
	clientTransport := NewUnixTransportWithContext(ctx, clientConn)
	seenSeqs := make([]int, 0, totalMessages)
	for i := 0; i < totalMessages; i++ {
		msg, readErr := clientTransport.ReadMessage()
		require.NoError(t, readErr)
		seenSeqs = append(seenSeqs, msg.GetSeq())
	}

	assert.Len(t, seenSeqs, totalMessages)
	// Verify monotonically increasing.
	for i := 1; i < len(seenSeqs); i++ {
		assert.Greater(t, seenSeqs[i], seenSeqs[i-1],
			"seq values must be monotonically increasing: seq[%d]=%d, seq[%d]=%d",
			i-1, seenSeqs[i-1], i, seenSeqs[i])
	}

	cancel()
	<-errCh
}

func TestMessagePipe_SeqMapPopulatedForRequests(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	transport := NewUnixTransportWithContext(ctx, serverConn)
	pipe := NewMessagePipe(ctx, transport, "test", logr.Discard())

	errCh := make(chan error, 1)
	go func() {
		errCh <- pipe.Run(ctx)
	}()

	// Send a request with original seq=42.
	env := NewMessageEnvelope(&dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 42, Type: "request"},
			Command:         "initialize",
		},
	})
	pipe.Send(env)

	// Also send an event (should NOT be stored in seqMap).
	eventEnv := NewMessageEnvelope(&dap.StoppedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{Seq: 99, Type: "event"},
			Event:           "stopped",
		},
	})
	pipe.Send(eventEnv)

	// Drain both messages from the transport.
	clientTransport := NewUnixTransportWithContext(ctx, clientConn)
	msg1, readErr1 := clientTransport.ReadMessage()
	require.NoError(t, readErr1)
	msg2, readErr2 := clientTransport.ReadMessage()
	require.NoError(t, readErr2)

	// The request should have been assigned seq=1, and the mapping 1→42 stored.
	assert.Equal(t, 1, msg1.GetSeq())
	origSeq, found := pipe.seqMap.Load(1)
	assert.True(t, found, "seqMap should contain mapping for request")
	assert.Equal(t, 42, origSeq)

	// The event (seq=2) should NOT be in the seqMap.
	assert.Equal(t, 2, msg2.GetSeq())
	_, eventFound := pipe.seqMap.Load(2)
	assert.False(t, eventFound, "seqMap should not contain mapping for events")

	cancel()
	<-errCh
}

func TestMessagePipe_RemapResponseSeq(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	// We don't need a real transport for this test — just the seqMap.
	pipe := NewMessagePipe(ctx, nil, "test", logr.Discard())

	// Manually populate the seqMap as if a request with virtualSeq=5 was written
	// and the original IDE seq was 42.
	pipe.seqMap.Store(5, 42)

	// Create a response envelope with request_seq=5 (the virtual seq).
	env := NewMessageEnvelope(&dap.InitializeResponse{
		Response: dap.Response{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "response"},
			RequestSeq:      5,
			Command:         "initialize",
			Success:         true,
		},
	})

	pipe.RemapResponseSeq(env)

	assert.Equal(t, 42, env.RequestSeq, "request_seq should be remapped to original IDE seq")

	// The mapping should be consumed (deleted).
	_, found := pipe.seqMap.Load(5)
	assert.False(t, found, "seqMap entry should be deleted after remap")
}

func TestMessagePipe_RemapResponseSeq_IgnoresNonResponses(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	pipe := NewMessagePipe(ctx, nil, "test", logr.Discard())
	pipe.seqMap.Store(1, 100)

	// Try to remap a request — should be a no-op.
	env := NewMessageEnvelope(&dap.ContinueRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "continue",
		},
	})

	pipe.RemapResponseSeq(env)

	// seqMap entry should still exist (not consumed).
	_, found := pipe.seqMap.Load(1)
	assert.True(t, found, "seqMap entry should not be consumed for non-response messages")
}

func TestMessagePipe_ContextCancellationStopsWriter(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)

	transport := NewUnixTransportWithContext(ctx, serverConn)
	pipe := NewMessagePipe(ctx, transport, "test", logr.Discard())

	errCh := make(chan error, 1)
	go func() {
		errCh <- pipe.Run(ctx)
	}()

	// Cancel the context — the writer should stop.
	cancel()

	select {
	case runErr := <-errCh:
		// Writer should return with context.Canceled (or nil if Out closed first).
		if runErr != nil {
			assert.ErrorIs(t, runErr, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine did not stop after context cancellation")
	}
}

func TestMessagePipe_SeqCounterContinuesAfterStop(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	ctx, cancel := testutil.GetTestContext(t, 5*time.Second)

	transport := NewUnixTransportWithContext(ctx, serverConn)
	pipe := NewMessagePipe(ctx, transport, "test", logr.Discard())

	errCh := make(chan error, 1)
	go func() {
		errCh <- pipe.Run(ctx)
	}()

	// Send a couple of messages so counter reaches 2.
	clientTransport := NewUnixTransportWithContext(ctx, clientConn)
	for i := 0; i < 2; i++ {
		env := NewMessageEnvelope(&dap.ContinueRequest{
			Request: dap.Request{
				ProtocolMessage: dap.ProtocolMessage{Seq: 1, Type: "request"},
				Command:         "continue",
			},
		})
		pipe.Send(env)

		_, readErr := clientTransport.ReadMessage()
		require.NoError(t, readErr)
	}

	// Stop the writer.
	cancel()
	<-errCh

	// SeqCounter should continue from where the writer left off.
	nextSeq := int(pipe.SeqCounter.Add(1))
	assert.Equal(t, 3, nextSeq, "SeqCounter should continue from where writer left off")
}
