/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that Bridge publishes connected, DAP milestone, debuggee-exit and endpoint-disconnection
// observations directly from real forwarding, without confusing debuggee exit with stream closure.
func TestBridgePublishesSessionObservations(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	clientPeer, clientBridge := net.Pipe()
	adapterPeer, adapterBridge := net.Pipe()
	for _, connection := range []net.Conn{clientPeer, clientBridge, adapterPeer, adapterBridge} {
		defer connection.Close()
		if deadline, present := ctx.Deadline(); present {
			require.NoError(t, connection.SetDeadline(deadline))
		}
	}
	client := &testTransport{Conn: clientPeer, reader: bufio.NewReader(clientPeer)}
	adapter := &testTransport{Conn: adapterPeer, reader: bufio.NewReader(adapterPeer)}
	var snapshots []SessionState
	done := make(chan error, 1)
	go func() {
		done <- Bridge(ctx,
			&testTransport{Conn: clientBridge, reader: bufio.NewReader(clientBridge)},
			&testTransport{Conn: adapterBridge, reader: bufio.NewReader(adapterBridge)}, nil, nil, time.Second, nil,
			func(state SessionState) error {
				snapshots = append(snapshots, state)
				return nil
			})
	}()
	require.NoError(t, client.WriteMessage(testRequest(1, "initialize")))
	initialize, readErr := adapter.ReadMessage()
	require.NoError(t, readErr)
	require.NoError(t, adapter.WriteMessage(testResponse(1, initialize.number("seq"), "initialize")))
	_, responseErr := client.ReadMessage()
	require.NoError(t, responseErr)
	for sequence, event := range []string{"initialized", "process", "exited"} {
		message := Message{"seq": json.RawMessage("1"), "type": json.RawMessage(`"event"`),
			"event": json.RawMessage(`"` + event + `"`)}
		message.setNumber("seq", int64(sequence+2))
		if event == "exited" {
			message["body"] = json.RawMessage(`{"exitCode":7}`)
		}
		require.NoError(t, adapter.WriteMessage(message))
		_, eventErr := client.ReadMessage()
		require.NoError(t, eventErr)
	}
	require.NoError(t, client.WriteMessage(testRequest(2, "disconnect")))
	disconnect, disconnectErr := adapter.ReadMessage()
	require.NoError(t, disconnectErr)
	require.NoError(t, adapter.WriteMessage(testResponse(5, disconnect.number("seq"), "disconnect")))
	_, acknowledgedErr := client.ReadMessage()
	require.NoError(t, acknowledgedErr)
	require.NoError(t, client.Close())
	select {
	case completionErr := <-done:
		require.NoError(t, completionErr)
	case <-ctx.Done():
		t.Fatal("bridge did not complete after client disconnect")
	}
	require.True(t, snapshots[0].StreamsConnected)
	last := snapshots[len(snapshots)-1]
	require.True(t, last.InitializeRequested)
	require.True(t, last.AdapterInitialized)
	require.True(t, last.ProcessReported)
	require.True(t, last.DebuggeeExited)
	require.EqualValues(t, 7, *last.DebuggeeExitCode)
	require.True(t, last.ClientTerminating)
	require.True(t, last.ClientDisconnected)
	require.True(t, last.AdapterDisconnected)
	require.Equal(t, ConnectionDisconnected, last.Phase)
	for _, snapshot := range snapshots {
		if snapshot.DebuggeeExited && !snapshot.ClientTerminating && !snapshot.ClientDisconnected {
			require.Equal(t, ConnectionConnected, snapshot.Phase)
		}
	}
}

// Verifies that sessionTracker latches independently ordered DAP milestones without request history,
// preserves debuggee exit separately, and does not reopen a terminating or disconnected stream.
func TestSessionTracksMonotonicObservations(t *testing.T) {
	t.Parallel()
	var snapshots []SessionState
	tracker := &sessionTracker{publish: func(state SessionState) error {
		snapshots = append(snapshots, state)
		return nil
	}}
	require.NoError(t, tracker.update(func(state *SessionState) error {
		state.Phase, state.StreamsConnected = ConnectionConnected, true
		return nil
	}))
	require.NoError(t, tracker.message(ClientToAdapter, testRequest(1, "launch")))
	require.NoError(t, tracker.message(AdapterToClient, Message{
		"type": json.RawMessage(`"event"`), "event": json.RawMessage(`"initialized"`),
	}))
	require.NoError(t, tracker.message(ClientToAdapter, testRequest(2, "initialize")))
	require.NoError(t, tracker.message(ClientToAdapter, testRequest(3, "attach")))
	require.NoError(t, tracker.message(AdapterToClient, Message{
		"type": json.RawMessage(`"event"`), "event": json.RawMessage(`"process"`),
	}))
	snapshotCount := len(snapshots)
	for sequence := int64(4); sequence < 20; sequence++ {
		require.NoError(t, tracker.message(ClientToAdapter, testRequest(sequence, "initialize")))
	}
	require.Len(t, snapshots, snapshotCount)
	require.True(t, tracker.state.LaunchRequested)
	require.True(t, tracker.state.AttachRequested)
	require.True(t, tracker.state.InitializeRequested)
	require.True(t, tracker.state.AdapterInitialized)
	require.True(t, tracker.state.ProcessReported)
	require.NoError(t, tracker.message(AdapterToClient, Message{
		"type": json.RawMessage(`"event"`), "event": json.RawMessage(`"exited"`), "body": json.RawMessage(`{"exitCode":7}`),
	}))
	require.Equal(t, ConnectionConnected, tracker.state.Phase)
	require.True(t, tracker.state.DebuggeeExited)
	require.EqualValues(t, 7, *tracker.state.DebuggeeExitCode)
	require.NoError(t, tracker.message(ClientToAdapter, testRequest(20, "terminate")))
	require.Equal(t, ConnectionTerminating, tracker.state.Phase)
	require.True(t, tracker.state.ClientTerminating)
	require.NoError(t, tracker.message(AdapterToClient, Message{
		"type": json.RawMessage(`"event"`), "event": json.RawMessage(`"terminated"`),
	}))
	require.True(t, tracker.state.AdapterTerminating)
	require.NoError(t, tracker.disconnect(true, "client closed"))
	require.NoError(t, tracker.message(ClientToAdapter, testRequest(21, "launch")))
	require.Equal(t, ConnectionDisconnected, tracker.state.Phase)
	require.Equal(t, "client closed", tracker.state.ClientDisconnectMessage)
	require.NoError(t, tracker.disconnect(false, "bridge closed adapter"))
	require.True(t, tracker.state.AdapterDisconnected)
	require.True(t, snapshots[len(snapshots)-1].StreamsConnected)
}
