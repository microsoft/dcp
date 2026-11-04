/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

type testTransport struct {
	net.Conn
	reader *bufio.Reader
}

func (transport *testTransport) ReadMessage() (Message, error) {
	return ReadMessage(transport.reader)
}

func (transport *testTransport) WriteMessage(message Message) error {
	return WriteMessage(transport, message)
}

func bridgeEndpoints(t *testing.T, ctx context.Context, grace time.Duration, exitSignals ...<-chan struct{}) (*testTransport, *testTransport, <-chan error) {
	t.Helper()
	client, bridgeClient := net.Pipe()
	adapter, bridgeAdapter := net.Pipe()
	for _, connection := range []net.Conn{client, bridgeClient, adapter, bridgeAdapter} {
		t.Cleanup(func() { _ = connection.Close() })
		if deadline, exists := ctx.Deadline(); exists {
			require.NoError(t, connection.SetDeadline(deadline))
		}
	}
	done := make(chan error, 1)
	var exited <-chan struct{}
	if len(exitSignals) != 0 {
		exited = exitSignals[0]
	}
	go func() {
		done <- Bridge(ctx,
			&testTransport{Conn: bridgeClient, reader: bufio.NewReader(bridgeClient)},
			&testTransport{Conn: bridgeAdapter, reader: bufio.NewReader(bridgeAdapter)}, exited, grace)
	}()
	return &testTransport{Conn: client, reader: bufio.NewReader(client)},
		&testTransport{Conn: adapter, reader: bufio.NewReader(adapter)}, done
}

func testRequest(sequence int64, command string) Message {
	message := Message{"type": json.RawMessage(`"request"`), "command": json.RawMessage(`"` + command + `"`)}
	message.setNumber("seq", sequence)
	return message
}

func testResponse(sequence, request int64, command string) Message {
	message := Message{
		"type": json.RawMessage(`"response"`), "command": json.RawMessage(`"` + command + `"`),
		"success": json.RawMessage("true"),
	}
	message.setNumber("seq", sequence)
	message.setNumber("request_seq", request)
	return message
}

// Verifies that serialized DAP mediation preserves extension fields and response correlation in
// both directions, including reverse requests with overlapping sequence spaces.
func TestBridgeRemapsBidirectionalRequests(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	client, adapter, done := bridgeEndpoints(t, ctx, time.Second)
	request := testRequest(71, "customExtension")
	request["extension"] = json.RawMessage(`{"nested":[1,"two"]}`)
	require.NoError(t, client.WriteMessage(request))
	forwarded, readErr := adapter.ReadMessage()
	require.NoError(t, readErr)
	require.EqualValues(t, 1, forwarded.number("seq"))
	require.JSONEq(t, `{"nested":[1,"two"]}`, string(forwarded["extension"]))
	cancellation := testRequest(72, "cancel")
	cancellation["arguments"] = json.RawMessage(`{"requestId":71,"progressId":"progress"}`)
	require.NoError(t, client.WriteMessage(cancellation))
	cancelRequest, cancelReadErr := adapter.ReadMessage()
	require.NoError(t, cancelReadErr)
	require.JSONEq(t, `{"requestId":1,"progressId":"progress"}`, string(cancelRequest["arguments"]))
	require.NoError(t, adapter.WriteMessage(testResponse(99, 1, "customExtension")))
	response, responseErr := client.ReadMessage()
	require.NoError(t, responseErr)
	require.EqualValues(t, 71, response.number("request_seq"))
	require.NoError(t, adapter.WriteMessage(testRequest(71, "runInTerminal")))
	reverse, reverseErr := client.ReadMessage()
	require.NoError(t, reverseErr)
	require.NoError(t, client.WriteMessage(testResponse(72, reverse.number("seq"), "runInTerminal")))
	reverseResponse, reverseResponseErr := adapter.ReadMessage()
	require.NoError(t, reverseResponseErr)
	require.EqualValues(t, 71, reverseResponse.number("request_seq"))
	require.NoError(t, adapter.Close())
	require.NoError(t, <-done)
}

// Verifies that deletion injects one ordered disconnect, consumes its private response, and
// forwards final adapter events before EOF without synthesizing completion messages.
func TestBridgeResourceShutdownDrainsFinalEvents(t *testing.T) {
	t.Parallel()
	lifetime, cancelLifetime := testutil.GetTestContext(t, time.Minute)
	defer cancelLifetime()
	ctx, cancel := context.WithCancelCause(lifetime)
	client, adapter, done := bridgeEndpoints(t, ctx, time.Second)
	cancel(ErrShutdownRequested)
	request, readErr := adapter.ReadMessage()
	require.NoError(t, readErr)
	require.Equal(t, "disconnect", request.text("command"))
	require.NoError(t, adapter.WriteMessage(testResponse(5, request.number("seq"), "disconnect")))
	require.NoError(t, adapter.WriteMessage(Message{
		"seq": json.RawMessage("6"), "type": json.RawMessage(`"event"`),
		"event": json.RawMessage(`"terminated"`), "body": json.RawMessage(`{"custom":true}`),
	}))
	event, eventErr := client.ReadMessage()
	require.NoError(t, eventErr)
	require.Equal(t, "terminated", event.text("event"))
	require.JSONEq(t, `{"custom":true}`, string(event["body"]))
	require.NoError(t, adapter.Close())
	require.NoError(t, <-done)
}

// Verifies that deletion does not inject another disconnect after a client shutdown request,
// while the client's response remains correctly correlated and final output drains.
func TestBridgePreservesClientShutdown(t *testing.T) {
	t.Parallel()
	lifetime, cancelLifetime := testutil.GetTestContext(t, time.Minute)
	defer cancelLifetime()
	ctx, cancel := context.WithCancelCause(lifetime)
	client, adapter, done := bridgeEndpoints(t, ctx, time.Second)
	require.NoError(t, client.WriteMessage(testRequest(40, "disconnect")))
	request, readErr := adapter.ReadMessage()
	require.NoError(t, readErr)
	cancel(ErrShutdownRequested)
	require.NoError(t, adapter.WriteMessage(testResponse(41, request.number("seq"), "disconnect")))
	response, responseErr := client.ReadMessage()
	require.NoError(t, responseErr)
	require.EqualValues(t, 40, response.number("request_seq"))
	require.NoError(t, adapter.Close())
	require.NoError(t, <-done)
}

// Verifies that unexpected client closure is a session failure, not successful protocol shutdown.
func TestBridgeUnexpectedDisconnectFails(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	client, _, done := bridgeEndpoints(t, ctx, time.Second)
	require.NoError(t, client.Close())
	require.ErrorContains(t, <-done, "client disconnected")
}

// Verifies that shutdown grace expires even when the adapter never reads the injected request,
// releasing blocked writes and joining the forwarding workers.
func TestBridgeShutdownBoundsBackpressure(t *testing.T) {
	t.Parallel()
	lifetime, cancelLifetime := testutil.GetTestContext(t, time.Minute)
	defer cancelLifetime()
	ctx, cancel := context.WithCancelCause(lifetime)
	_, _, done := bridgeEndpoints(t, ctx, 20*time.Millisecond)
	cancel(ErrShutdownRequested)
	select {
	case bridgeErr := <-done:
		require.ErrorContains(t, bridgeErr, "shutdown timed out")
	case <-lifetime.Done():
		t.Fatal("shutdown failed to release blocked adapter write")
	}
}

// Verifies that shutdown grace also bounds a client that stops reading final adapter output.
func TestBridgeShutdownBoundsClientBackpressure(t *testing.T) {
	t.Parallel()
	lifetime, cancelLifetime := testutil.GetTestContext(t, time.Minute)
	defer cancelLifetime()
	ctx, cancel := context.WithCancelCause(lifetime)
	_, adapter, done := bridgeEndpoints(t, ctx, 20*time.Millisecond)
	require.NoError(t, adapter.WriteMessage(Message{
		"seq": json.RawMessage("1"), "type": json.RawMessage(`"event"`), "event": json.RawMessage(`"output"`),
	}))
	cancel(ErrShutdownRequested)
	request, readErr := adapter.ReadMessage()
	require.NoError(t, readErr)
	require.Equal(t, "disconnect", request.text("command"))
	select {
	case bridgeErr := <-done:
		require.ErrorContains(t, bridgeErr, "shutdown timed out")
	case <-lifetime.Done():
		t.Fatal("shutdown failed to release blocked client write")
	}
}

// Verifies that process exit cannot leave a final-output write blocked on a non-reading client.
func TestBridgeBoundsPostExitDrain(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	exited := make(chan struct{})
	_, adapter, done := bridgeEndpoints(t, ctx, time.Second, exited)
	require.NoError(t, adapter.WriteMessage(Message{
		"seq": json.RawMessage("1"), "type": json.RawMessage(`"event"`), "event": json.RawMessage(`"terminated"`),
	}))
	close(exited)
	select {
	case bridgeErr := <-done:
		require.ErrorContains(t, bridgeErr, "output drain timed out")
	case <-ctx.Done():
		t.Fatal("adapter exit left output forwarding blocked")
	}
}

// Verifies that framing rejects excessive or invalid lengths and preserves unknown payloads.
func TestMessageFramingValidation(t *testing.T) {
	t.Parallel()
	for _, frame := range []string{
		"Content-Length: 8388609\r\n\r\n",
		"Content-Length: -1\r\n\r\n",
		"Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}",
		"Content-Length: 2\r\n\r\n{}",
	} {
		_, readErr := ReadMessage(bufio.NewReader(strings.NewReader(frame)))
		require.Error(t, readErr)
	}
	_, emptyErr := ReadMessage(bufio.NewReader(strings.NewReader("")))
	require.True(t, errors.Is(emptyErr, io.EOF))
	for _, truncated := range []string{"Content-Len", "Content-Length: 4\r\n", "Content-Length: 4\r\n\r\n"} {
		_, truncatedErr := ReadMessage(bufio.NewReader(strings.NewReader(truncated)))
		require.ErrorIs(t, truncatedErr, io.ErrUnexpectedEOF)
	}
}

// Verifies that an adapter rejection of DCP's disconnect is surfaced rather than swallowed.
func TestBridgeReportsRejectedShutdown(t *testing.T) {
	t.Parallel()
	lifetime, cancelLifetime := testutil.GetTestContext(t, time.Minute)
	defer cancelLifetime()
	ctx, cancel := context.WithCancelCause(lifetime)
	_, adapter, done := bridgeEndpoints(t, ctx, time.Second)
	cancel(ErrShutdownRequested)
	request, readErr := adapter.ReadMessage()
	require.NoError(t, readErr)
	response := testResponse(2, request.number("seq"), "disconnect")
	response["success"], response["message"] = json.RawMessage("false"), json.RawMessage(`"disconnect denied"`)
	require.NoError(t, adapter.WriteMessage(response))
	require.ErrorContains(t, <-done, "disconnect denied")
}
