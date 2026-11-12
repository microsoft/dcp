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
	"fmt"
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

func bridgeEndpoints(t *testing.T, ctx context.Context, grace time.Duration, observe func(Direction, Message) error, exitSignals ...<-chan struct{}) (*testTransport, *testTransport, <-chan error) {
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
			&testTransport{Conn: bridgeAdapter, reader: bufio.NewReader(bridgeAdapter)}, exited, nil, grace, observe)
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

// Verifies that observers see original client correlation before forwarding, including out-of-order
// replies, without consuming extension payloads or observing unrelated/private responses.
func TestBridgeObservesCorrelatedProtocolMessages(t *testing.T) {
	t.Parallel()
	lifetime, cancelLifetime := testutil.GetTestContext(t, time.Minute)
	defer cancelLifetime()
	ctx, cancel := context.WithCancelCause(lifetime)
	type observation struct {
		direction Direction
		command   string
		sequence  int64
		request   int64
	}
	observed := make(chan observation, 8)
	client, adapter, done := bridgeEndpoints(t, ctx, time.Second, func(direction Direction, message Message) error {
		observed <- observation{direction, message.text("command"), message.number("seq"), message.number("request_seq")}
		return nil
	})
	for index, command := range []string{"initialize", "launch"} {
		request := testRequest(int64(71+index), command)
		request["arguments"] = json.RawMessage(`{"unknown":{"preserve":true},"noDebug":true}`)
		require.NoError(t, client.WriteMessage(request))
		forwarded, readErr := adapter.ReadMessage()
		require.NoError(t, readErr)
		require.EqualValues(t, index+1, forwarded.number("seq"))
		require.JSONEq(t, `{"unknown":{"preserve":true},"noDebug":true}`, string(forwarded["arguments"]))
	}
	require.NoError(t, adapter.WriteMessage(testResponse(4, 71, "initialize")))
	_, unmatchedErr := client.ReadMessage()
	require.NoError(t, unmatchedErr)
	for _, response := range []Message{testResponse(5, 2, "launch"), testResponse(6, 1, "initialize")} {
		require.NoError(t, adapter.WriteMessage(response))
		_, readErr := client.ReadMessage()
		require.NoError(t, readErr)
	}
	cancel(ErrShutdownRequested)
	shutdown, shutdownErr := adapter.ReadMessage()
	require.NoError(t, shutdownErr)
	require.NoError(t, adapter.WriteMessage(testResponse(7, shutdown.number("seq"), "disconnect")))
	require.NoError(t, adapter.WriteMessage(Message{"seq": json.RawMessage("8"), "type": json.RawMessage(`"event"`), "event": json.RawMessage(`"terminated"`)}))
	_, eventErr := client.ReadMessage()
	require.NoError(t, eventErr)
	require.NoError(t, adapter.Close())
	require.NoError(t, <-done)
	require.Len(t, observed, 5)
	for index, expected := range []observation{
		{direction: ClientToAdapter, command: "initialize", sequence: 71},
		{direction: ClientToAdapter, command: "launch", sequence: 72},
		{direction: AdapterToClient, command: "launch", request: 72},
		{direction: AdapterToClient, command: "initialize", request: 71},
		{direction: AdapterToClient},
	} {
		actual := <-observed
		require.Equal(t, expected.direction, actual.direction, "observation %d", index)
		require.Equal(t, expected.command, actual.command)
		require.Equal(t, expected.request, actual.request)
		if expected.direction == ClientToAdapter {
			require.Equal(t, expected.sequence, actual.sequence)
		}
	}
}

// Verifies that observation failures stop forwarding explicitly and release both transport workers.
func TestBridgeObservationFailureClosesTransports(t *testing.T) {
	t.Parallel()
	for _, direction := range []Direction{ClientToAdapter, AdapterToClient} {
		ctx, cancel := testutil.GetTestContext(t, time.Minute)
		client, adapter, done := bridgeEndpoints(t, ctx, time.Second, func(origin Direction, _ Message) error {
			if origin == direction {
				return errors.New("injected observation failure")
			}
			return nil
		})
		if direction == ClientToAdapter {
			require.NoError(t, client.WriteMessage(testRequest(1, "initialize")))
		} else {
			require.NoError(t, adapter.WriteMessage(Message{"seq": json.RawMessage("1"), "type": json.RawMessage(`"event"`), "event": json.RawMessage(`"output"`)}))
		}
		require.ErrorContains(t, <-done, "injected observation failure")
		cancel()
	}
}

// Verifies that serialized DAP mediation preserves extension fields and response correlation in
// both directions, including reverse requests with overlapping sequence spaces.
func TestBridgeRemapsBidirectionalRequests(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	client, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil)
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

// Verifies that Bridge translates progress and cancellation request references in both directions,
// preserves opaque IDs, and retains request mappings until their out-of-order responses arrive.
func TestBridgeRemapsProgressAndCancellation(t *testing.T) {
	t.Parallel()
	for _, direction := range []Direction{ClientToAdapter, AdapterToClient} {
		t.Run(fmt.Sprint(direction), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			client, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil)
			source, destination := client, adapter
			if direction == AdapterToClient {
				source, destination = adapter, client
			}
			for index := int64(0); index < 2; index++ {
				require.NoError(t, source.WriteMessage(testRequest(71+index, "customExtension")))
				request, requestErr := destination.ReadMessage()
				require.NoError(t, requestErr)
				require.EqualValues(t, index+1, request.number("seq"))
			}
			progress := Message{
				"seq": json.RawMessage("91"), "type": json.RawMessage(`"event"`),
				"event": json.RawMessage(`"progressStart"`),
				"body":  json.RawMessage(`{"progressId":"2","requestId":2,"title":"Working","cancellable":true,"extension":{"requestId":2}}`),
			}
			require.NoError(t, destination.WriteMessage(progress))
			forwardedProgress, progressErr := source.ReadMessage()
			require.NoError(t, progressErr)
			require.JSONEq(t, `{"progressId":"2","requestId":72,"title":"Working","cancellable":true,"extension":{"requestId":2}}`,
				string(forwardedProgress["body"]))
			cancellation := testRequest(73, "cancel")
			cancellation["arguments"] = json.RawMessage(`{"requestId":72,"progressId":"2","extension":{"requestId":72}}`)
			require.NoError(t, source.WriteMessage(cancellation))
			forwardedCancel, cancelErr := destination.ReadMessage()
			require.NoError(t, cancelErr)
			require.JSONEq(t, `{"requestId":2,"progressId":"2","extension":{"requestId":72}}`, string(forwardedCancel["arguments"]))
			require.NoError(t, destination.WriteMessage(testResponse(92, forwardedCancel.number("seq"), "cancel")))
			cancelResponse, cancelResponseErr := source.ReadMessage()
			require.NoError(t, cancelResponseErr)
			require.EqualValues(t, 73, cancelResponse.number("request_seq"))
			require.NoError(t, destination.WriteMessage(progress))
			repeatedProgress, repeatedProgressErr := source.ReadMessage()
			require.NoError(t, repeatedProgressErr)
			require.JSONEq(t, string(forwardedProgress["body"]), string(repeatedProgress["body"]))
			for _, request := range []int64{2, 1} {
				require.NoError(t, destination.WriteMessage(testResponse(93+request, request, "customExtension")))
				response, responseErr := source.ReadMessage()
				require.NoError(t, responseErr)
				require.EqualValues(t, 70+request, response.number("request_seq"))
			}
			require.NoError(t, destination.WriteMessage(progress))
			unmatchedProgress, unmatchedProgressErr := source.ReadMessage()
			require.NoError(t, unmatchedProgressErr)
			require.JSONEq(t, `{"progressId":"2","title":"Working","cancellable":true,"extension":{"requestId":2}}`,
				string(unmatchedProgress["body"]))
			progress["body"] = json.RawMessage(`{"progressId":"2","title":"General work","threadId":2,"extension":{"requestId":2}}`)
			require.NoError(t, destination.WriteMessage(progress))
			unassociatedProgress, unassociatedProgressErr := source.ReadMessage()
			require.NoError(t, unassociatedProgressErr)
			require.JSONEq(t, string(progress["body"]), string(unassociatedProgress["body"]))
			cancellation.setNumber("seq", 74)
			require.NoError(t, source.WriteMessage(cancellation))
			staleCancel, staleCancelErr := destination.ReadMessage()
			require.NoError(t, staleCancelErr)
			require.JSONEq(t, `{"progressId":"2","extension":{"requestId":72}}`, string(staleCancel["arguments"]))
			require.NoError(t, adapter.Close())
			require.NoError(t, <-done)
		})
	}
}

// Verifies that Bridge rejects malformed standard request references explicitly in progress
// events and cancellation requests rather than forwarding invalid or ambiguous correlations.
func TestBridgeRejectsInvalidRequestReferences(t *testing.T) {
	t.Parallel()
	for _, cancellation := range []bool{false, true} {
		for _, reference := range []string{`"invalid"`, `null`, `0`, `-1`, `1.5`} {
			t.Run(fmt.Sprintf("cancel=%t/requestId=%s", cancellation, reference), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := testutil.GetTestContext(t, 0)
				defer cancel()
				client, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil)
				properties := json.RawMessage(`{"requestId":` + reference + `}`)
				if cancellation {
					message := testRequest(71, "cancel")
					message["arguments"] = properties
					require.NoError(t, client.WriteMessage(message))
				} else {
					require.NoError(t, adapter.WriteMessage(Message{
						"seq": json.RawMessage("71"), "type": json.RawMessage(`"event"`),
						"event": json.RawMessage(`"progressStart"`), "body": properties,
					}))
				}
				require.ErrorContains(t, <-done, "requestId")
			})
		}
	}
}

// Verifies that deletion injects one ordered disconnect, consumes its private response, and
// forwards final adapter events before EOF without synthesizing completion messages.
func TestBridgeResourceShutdownDrainsFinalEvents(t *testing.T) {
	t.Parallel()
	lifetime, cancelLifetime := testutil.GetTestContext(t, time.Minute)
	defer cancelLifetime()
	ctx, cancel := context.WithCancelCause(lifetime)
	client, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil)
	cancel(ErrShutdownRequested)
	request, readErr := adapter.ReadMessage()
	require.NoError(t, readErr)
	require.Equal(t, "disconnect", request.text("command"))
	progress := Message{
		"seq": json.RawMessage("4"), "type": json.RawMessage(`"event"`),
		"event": json.RawMessage(`"progressStart"`),
		"body":  json.RawMessage(fmt.Sprintf(`{"progressId":"shutdown","title":"Disconnecting","requestId":%d}`, request.number("seq"))),
	}
	require.NoError(t, adapter.WriteMessage(progress))
	forwardedProgress, progressErr := client.ReadMessage()
	require.NoError(t, progressErr)
	require.JSONEq(t, `{"progressId":"shutdown","title":"Disconnecting"}`, string(forwardedProgress["body"]))
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
	client, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil)
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

// Verifies that Bridge continues forwarding after terminate and permits either a client
// disconnect or an injected resource-shutdown disconnect with correct response correlation.
func TestBridgeTerminateAllowsDisconnect(t *testing.T) {
	t.Parallel()
	for _, injected := range []bool{false, true} {
		name := "client"
		if injected {
			name = "resource"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lifetime, cancelLifetime := testutil.GetTestContext(t, time.Minute)
			defer cancelLifetime()
			ctx, cancel := context.WithCancelCause(lifetime)
			defer cancel(nil)
			client, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil)
			require.NoError(t, client.WriteMessage(testRequest(40, "terminate")))
			terminate, terminateErr := adapter.ReadMessage()
			require.NoError(t, terminateErr)
			require.Equal(t, "terminate", terminate.text("command"))
			require.NoError(t, adapter.WriteMessage(testResponse(50, terminate.number("seq"), "terminate")))
			terminateResponse, terminateResponseErr := client.ReadMessage()
			require.NoError(t, terminateResponseErr)
			require.EqualValues(t, 40, terminateResponse.number("request_seq"))
			require.NoError(t, adapter.WriteMessage(Message{
				"seq": json.RawMessage("51"), "type": json.RawMessage(`"event"`),
				"event": json.RawMessage(`"terminated"`),
			}))
			terminated, terminatedErr := client.ReadMessage()
			require.NoError(t, terminatedErr)
			require.Equal(t, "terminated", terminated.text("event"))
			require.NoError(t, client.WriteMessage(testRequest(41, "threads")))
			threads, threadsErr := adapter.ReadMessage()
			require.NoError(t, threadsErr)
			require.Equal(t, "threads", threads.text("command"))
			require.NoError(t, adapter.WriteMessage(testResponse(52, threads.number("seq"), "threads")))
			threadsResponse, threadsResponseErr := client.ReadMessage()
			require.NoError(t, threadsResponseErr)
			require.EqualValues(t, 41, threadsResponse.number("request_seq"))
			if injected {
				cancel(ErrShutdownRequested)
			} else {
				require.NoError(t, client.WriteMessage(testRequest(42, "disconnect")))
			}
			disconnect, disconnectErr := adapter.ReadMessage()
			require.NoError(t, disconnectErr)
			require.Equal(t, "disconnect", disconnect.text("command"))
			require.NoError(t, adapter.WriteMessage(testResponse(53, disconnect.number("seq"), "disconnect")))
			if !injected {
				disconnectResponse, disconnectResponseErr := client.ReadMessage()
				require.NoError(t, disconnectResponseErr)
				require.EqualValues(t, 42, disconnectResponse.number("request_seq"))
			}
			require.NoError(t, adapter.Close())
			require.NoError(t, <-done)
		})
	}
}

// Verifies that Bridge completes normally when the client closes after an acknowledged disconnect
// while the adapter connection remains open.
func TestBridgeClientClosureAfterAcknowledgedDisconnect(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	client, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil)
	require.NoError(t, client.WriteMessage(testRequest(40, "disconnect")))
	request, requestErr := adapter.ReadMessage()
	require.NoError(t, requestErr)
	require.Equal(t, "disconnect", request.text("command"))
	require.NoError(t, adapter.WriteMessage(testResponse(41, request.number("seq"), "disconnect")))
	response, responseErr := client.ReadMessage()
	require.NoError(t, responseErr)
	require.EqualValues(t, 40, response.number("request_seq"))
	require.NoError(t, client.Close())
	require.NoError(t, <-done)
}

// Verifies that unexpected client closure is a session failure, not successful protocol shutdown.
func TestBridgeUnexpectedDisconnectFails(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	client, _, done := bridgeEndpoints(t, ctx, time.Second, nil)
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
	_, _, done := bridgeEndpoints(t, ctx, 20*time.Millisecond, nil)
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
	_, adapter, done := bridgeEndpoints(t, ctx, 20*time.Millisecond, nil)
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
	_, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil, exited)
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
	_, adapter, done := bridgeEndpoints(t, ctx, time.Second, nil)
	cancel(ErrShutdownRequested)
	request, readErr := adapter.ReadMessage()
	require.NoError(t, readErr)
	response := testResponse(2, request.number("seq"), "disconnect")
	response["success"], response["message"] = json.RawMessage("false"), json.RawMessage(`"disconnect denied"`)
	require.NoError(t, adapter.WriteMessage(response))
	require.ErrorContains(t, <-done, "disconnect denied")
}
