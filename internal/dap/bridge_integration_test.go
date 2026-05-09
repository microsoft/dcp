/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-dap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/testutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	pkgtestutil "github.com/microsoft/dcp/pkg/testutil"
)

// ===== Integration Tests =====

func TestBridge_RunWithConnection(t *testing.T) {
	t.Parallel()

	// Test that RunWithConnection works correctly with an already-connected net.Conn
	// This simulates the flow where BridgeSocketManager has already performed handshake

	// We'll use a pipe to simulate the connection
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	sessionID := "test-session"

	config := BridgeConfig{
		SessionID: sessionID,
		AdapterConfig: &DebugAdapterConfig{
			Args: []string{"echo", "hello"}, // Simple command that exits immediately
			Mode: DebugAdapterModeStdio,
		},
		Logger: logr.Discard(),
	}

	bridge := NewDapBridge(config)

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	// Drain clientConn so the bridge can write error messages to the IDE transport
	// without blocking on the pipe.
	go func() {
		_, _ = io.Copy(io.Discard, clientConn)
	}()

	// Run the bridge in a goroutine - it will fail to launch the adapter since we're using a fake command
	// but this tests the basic flow
	go func() {
		_ = bridge.RunWithConnection(ctx, serverConn)
	}()

	// Wait for the bridge to terminate (it will fail to launch the fake adapter and exit)
	select {
	case <-bridge.terminateCh:
		// Expected - bridge terminated after failing to launch adapter
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("bridge did not terminate in time")
	}
}

func TestBridgeManager_HandshakeValidation(t *testing.T) {
	t.Parallel()

	// Test that BridgeManager correctly validates handshakes

	socketDir := shortTempDir(t)
	manager := NewBridgeManager(BridgeManagerConfig{
		SocketDir:        socketDir,
		HandshakeTimeout: 2 * time.Second,
	}, logr.Discard())

	// Register a session with a token
	session, regErr := manager.RegisterSession("valid-session", "test-token")
	require.NoError(t, regErr)
	require.NotNil(t, session)

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	// Run bridge manager in background
	go func() {
		_ = manager.Run(ctx)
	}()

	// Wait for it to be ready
	select {
	case <-manager.Ready():
		// Good
	case <-time.After(2 * time.Second):
		t.Fatal("bridge manager failed to become ready")
	}

	socketPath, socketPathErr := manager.SocketPath(ctx)
	require.NoError(t, socketPathErr)

	// Connect with wrong token - should fail
	ideConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)
	defer ideConn.Close()

	handshakeErr := performClientHandshake(ideConn, "wrong-token", "valid-session", "")
	require.Error(t, handshakeErr, "handshake should fail with wrong token")
	assert.ErrorIs(t, handshakeErr, ErrHandshakeFailed)

	cancel()
}

func TestBridgeManager_SessionNotFound(t *testing.T) {
	t.Parallel()

	// Test handshake failure when session doesn't exist

	socketDir := shortTempDir(t)
	manager := NewBridgeManager(BridgeManagerConfig{
		SocketDir:        socketDir,
		HandshakeTimeout: 2 * time.Second,
	}, logr.Discard())

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	// Run bridge manager in background
	go func() {
		_ = manager.Run(ctx)
	}()

	// Wait for it to be ready
	select {
	case <-manager.Ready():
		// Good
	case <-time.After(2 * time.Second):
		t.Fatal("bridge manager failed to become ready")
	}

	socketPath, socketPathErr := manager.SocketPath(ctx)
	require.NoError(t, socketPathErr)

	// Connect with non-existent session - should fail
	ideConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)
	defer ideConn.Close()

	handshakeErr := performClientHandshake(ideConn, "any-token", "nonexistent-session", "")
	require.Error(t, handshakeErr, "handshake should fail with unknown session")
	assert.ErrorIs(t, handshakeErr, ErrHandshakeFailed)

	cancel()
}

func TestBridgeManager_HandshakeTimeout(t *testing.T) {
	t.Parallel()

	socketDir := shortTempDir(t)
	manager := NewBridgeManager(BridgeManagerConfig{
		SocketDir:        socketDir,
		HandshakeTimeout: 200 * time.Millisecond, // Short timeout
	}, logr.Discard())
	_, _ = manager.RegisterSession("timeout-session", "test-token")

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	// Run bridge manager in background
	go func() {
		_ = manager.Run(ctx)
	}()

	// Wait for it to be ready
	select {
	case <-manager.Ready():
		// Good
	case <-time.After(2 * time.Second):
		t.Fatal("bridge manager failed to become ready")
	}

	socketPath, socketPathErr := manager.SocketPath(ctx)
	require.NoError(t, socketPathErr)

	// Connect but don't send handshake - should timeout and close connection
	ideConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)
	defer ideConn.Close()

	// Poll until the server closes our connection due to handshake timeout
	pollErr := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(_ context.Context) (bool, error) {
		_ = ideConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		buf := make([]byte, 1)
		_, readErr := ideConn.Read(buf)
		// Connection closed when read returns a non-timeout error (EOF, closed, etc.)
		if readErr != nil {
			if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
				return false, nil // Still open, keep polling
			}
			return true, nil // Non-timeout error means connection was closed
		}
		return false, nil
	})
	require.NoError(t, pollErr, "connection should be closed by server after handshake timeout")

	cancel()
}

func TestBridge_OutputEventCapture(t *testing.T) {
	t.Parallel()

	// This test verifies that output events are captured via the OutputHandler.

	stdoutBuf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}

	config := BridgeConfig{
		SessionID:    "session",
		StdoutWriter: stdoutBuf,
		StderrWriter: stderrBuf,
		OutputHandler: &testOutputHandler{
			stdout: stdoutBuf,
			stderr: stderrBuf,
		},
	}

	bridge := NewDapBridge(config)

	// Simulate handling an output event
	outputEvent := &dap.OutputEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "event",
			},
			Event: "output",
		},
		Body: dap.OutputEventBody{
			Category: "stdout",
			Output:   "Hello from debug adapter\n",
		},
	}

	bridge.handleOutputEvent(outputEvent)

	// Output should have been captured
	assert.Contains(t, stdoutBuf.String(), "Hello from debug adapter")
}

// testOutputHandler captures output for testing.
type testOutputHandler struct {
	stdout io.Writer
	stderr io.Writer
}

func (h *testOutputHandler) HandleOutput(category string, output string) {
	if category == "stdout" && h.stdout != nil {
		_, _ = h.stdout.Write([]byte(output))
	} else if category == "stderr" && h.stderr != nil {
		_, _ = h.stderr.Write([]byte(output))
	}
}

func TestBridge_InitializeInterception(t *testing.T) {
	t.Parallel()

	// Test that the bridge forwards initialize requests without modifying
	// supportsRunInTerminalRequest (runInTerminal is not handled locally)

	config := BridgeConfig{
		SessionID: "session",
	}

	bridge := NewDapBridge(config)

	initReq := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "request",
			},
			Command: "initialize",
		},
		Arguments: dap.InitializeRequestArguments{
			ClientID:                     "test",
			SupportsRunInTerminalRequest: false,
		},
	}

	// Apply upstream interception
	modified, forward := bridge.interceptUpstreamMessage(initReq)

	assert.True(t, forward, "initialize should be forwarded")
	modifiedInit, ok := modified.(*dap.InitializeRequest)
	require.True(t, ok)
	assert.False(t, modifiedInit.Arguments.SupportsRunInTerminalRequest,
		"supportsRunInTerminalRequest should not be modified")
}

func TestBridge_RunInTerminalForwarded(t *testing.T) {
	t.Parallel()

	// Test that runInTerminal requests are forwarded to the IDE (not handled locally)

	config := BridgeConfig{
		SessionID: "session",
	}

	bridge := NewDapBridge(config)

	// Create a runInTerminal request
	ritReq := &dap.RunInTerminalRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "request",
			},
			Command: "runInTerminal",
		},
		Arguments: dap.RunInTerminalRequestArguments{
			Kind:  "integrated",
			Title: "Debug",
			Cwd:   "/tmp",
			Args:  []string{"echo", "hello"},
		},
	}

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	// Apply downstream interception
	modified, forward, asyncResponse := bridge.interceptDownstreamMessage(ctx, ritReq)

	assert.True(t, forward, "runInTerminal should be forwarded to IDE")
	assert.Equal(t, ritReq, modified)
	assert.Nil(t, asyncResponse, "should not return an async response")
}

func TestBridge_InitializeObservesStartDebuggingCapability(t *testing.T) {
	t.Parallel()

	config := BridgeConfig{
		SessionID: "session",
	}

	bridge := NewDapBridge(config)

	// When client sets supportsStartDebuggingRequest=true, bridge should observe it
	initReq := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "request",
			},
			Command: "initialize",
		},
		Arguments: dap.InitializeRequestArguments{
			ClientID:                        "test",
			SupportsStartDebuggingRequest:   true,
			SupportsRunInTerminalRequest:    false,
		},
	}

	modified, forward := bridge.interceptUpstreamMessage(initReq)

	assert.True(t, forward)
	modifiedInit := modified.(*dap.InitializeRequest)
	// Bridge should NOT modify supportsStartDebuggingRequest
	assert.True(t, modifiedInit.Arguments.SupportsStartDebuggingRequest,
		"supportsStartDebuggingRequest should not be modified")
	// Bridge should NOT modify supportsRunInTerminalRequest
	assert.False(t, modifiedInit.Arguments.SupportsRunInTerminalRequest,
		"supportsRunInTerminalRequest should not be modified")
	// Bridge should have recorded the capability
	assert.True(t, bridge.clientSupportsStartDebugging.Load())
}

func TestBridge_InitializeDoesNotSetStartDebuggingCapability(t *testing.T) {
	t.Parallel()

	config := BridgeConfig{
		SessionID: "session",
	}

	bridge := NewDapBridge(config)

	// When client does NOT set supportsStartDebuggingRequest, bridge should not add it
	initReq := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "request",
			},
			Command: "initialize",
		},
		Arguments: dap.InitializeRequestArguments{
			ClientID: "test",
		},
	}

	modified, forward := bridge.interceptUpstreamMessage(initReq)

	assert.True(t, forward)
	modifiedInit := modified.(*dap.InitializeRequest)
	assert.False(t, modifiedInit.Arguments.SupportsStartDebuggingRequest,
		"bridge should NOT set supportsStartDebuggingRequest")
	assert.False(t, bridge.clientSupportsStartDebugging.Load())
}

func TestBridge_StartDebuggingInterception_WithHandler(t *testing.T) {
	t.Parallel()

	handlerCalled := false
	config := BridgeConfig{
		SessionID: "parent-session",
		StartDebuggingHandler: func(parentSessionID string, configuration map[string]any, request string) (string, error) {
			handlerCalled = true
			assert.Equal(t, "parent-session", parentSessionID)
			assert.Equal(t, "launch", request)
			assert.Equal(t, "node", configuration["type"])
			return "parent-session:0", nil
		},
	}

	bridge := NewDapBridge(config)
	bridge.clientSupportsStartDebugging.Store(true)

	sdReq := &dap.StartDebuggingRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  5,
				Type: "request",
			},
			Command: "startDebugging",
		},
		Arguments: dap.StartDebuggingRequestArguments{
			Request:       "launch",
			Configuration: map[string]any{"type": "node", "program": "app.js"},
		},
	}

	modifiedMsg, forward, asyncResponse := bridge.handleStartDebuggingRequest(sdReq)

	assert.True(t, handlerCalled, "handler should be called")
	assert.True(t, forward, "request should be forwarded to IDE")

	// Verify the configuration was augmented with child session ID
	modifiedReq, ok := modifiedMsg.(*dap.StartDebuggingRequest)
	require.True(t, ok)
	assert.Equal(t, "parent-session:0", modifiedReq.Arguments.Configuration[childSessionIDKey])
	// Original config should be preserved
	assert.Equal(t, "node", modifiedReq.Arguments.Configuration["type"])
	assert.Equal(t, "app.js", modifiedReq.Arguments.Configuration["program"])

	// No async response — the IDE sends the response back to the adapter
	assert.Nil(t, asyncResponse, "bridge should not generate a response")
}

func TestBridge_StartDebuggingInterception_NoHandler(t *testing.T) {
	t.Parallel()

	// When no handler is configured, startDebugging should be forwarded unchanged
	config := BridgeConfig{
		SessionID: "session",
	}

	bridge := NewDapBridge(config)
	bridge.clientSupportsStartDebugging.Store(true)

	sdReq := &dap.StartDebuggingRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "request",
			},
			Command: "startDebugging",
		},
		Arguments: dap.StartDebuggingRequestArguments{
			Request:       "launch",
			Configuration: map[string]any{"type": "node"},
		},
	}

	modifiedMsg, forward, asyncResponse := bridge.handleStartDebuggingRequest(sdReq)

	assert.True(t, forward, "should be forwarded when no handler")
	assert.Equal(t, sdReq, modifiedMsg, "message should not be modified")
	assert.Nil(t, asyncResponse, "no async response when not intercepted")
}

func TestBridge_StartDebuggingInterception_ClientDoesNotSupport(t *testing.T) {
	t.Parallel()

	handlerCalled := false
	config := BridgeConfig{
		SessionID: "session",
		StartDebuggingHandler: func(string, map[string]any, string) (string, error) {
			handlerCalled = true
			return "child", nil
		},
	}

	bridge := NewDapBridge(config)
	// Client did NOT set supportsStartDebuggingRequest

	sdReq := &dap.StartDebuggingRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "request",
			},
			Command: "startDebugging",
		},
		Arguments: dap.StartDebuggingRequestArguments{
			Request: "launch",
		},
	}

	modifiedMsg, forward, asyncResponse := bridge.handleStartDebuggingRequest(sdReq)

	assert.False(t, handlerCalled, "handler should NOT be called when client doesn't support it")
	assert.True(t, forward, "should be forwarded unchanged")
	assert.Equal(t, sdReq, modifiedMsg)
	assert.Nil(t, asyncResponse)
}

func TestBridge_StartDebuggingInterception_HandlerError(t *testing.T) {
	t.Parallel()

	config := BridgeConfig{
		SessionID: "session",
		StartDebuggingHandler: func(string, map[string]any, string) (string, error) {
			return "", fmt.Errorf("registration failed")
		},
	}

	bridge := NewDapBridge(config)
	bridge.clientSupportsStartDebugging.Store(true)

	sdReq := &dap.StartDebuggingRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  3,
				Type: "request",
			},
			Command: "startDebugging",
		},
		Arguments: dap.StartDebuggingRequestArguments{
			Request: "launch",
		},
	}

	modifiedMsg, forward, asyncResponse := bridge.handleStartDebuggingRequest(sdReq)

	// On handler error, the request is forwarded unchanged (without bridge metadata)
	// so the IDE can handle it or fail naturally
	assert.True(t, forward, "should still be forwarded on handler error")
	assert.Equal(t, sdReq, modifiedMsg, "message should be unmodified")
	assert.Nil(t, asyncResponse, "no async response")
	assert.NotContains(t, sdReq.Arguments.Configuration, childSessionIDKey,
		"child session ID should not be injected on error")
}

func TestBridge_MessageForwarding(t *testing.T) {
	t.Parallel()

	// Test that non-intercepted messages are forwarded unchanged

	config := BridgeConfig{
		SessionID: "session",
	}

	bridge := NewDapBridge(config)

	// Test upstream message (setBreakpoints - should pass through)
	setBreakpointsReq := &dap.SetBreakpointsRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "request",
			},
			Command: "setBreakpoints",
		},
	}

	modified, forward := bridge.interceptUpstreamMessage(setBreakpointsReq)
	assert.True(t, forward, "setBreakpoints should be forwarded")
	assert.Equal(t, setBreakpointsReq, modified, "message should not be modified")

	// Test downstream message (stopped event - should pass through)
	stoppedEvent := &dap.StoppedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  2,
				Type: "event",
			},
			Event: "stopped",
		},
		Body: dap.StoppedEventBody{
			Reason:   "breakpoint",
			ThreadId: 1,
		},
	}

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	modifiedDown, forwardDown, asyncResp := bridge.interceptDownstreamMessage(ctx, stoppedEvent)
	assert.True(t, forwardDown, "stopped event should be forwarded")
	assert.Equal(t, stoppedEvent, modifiedDown, "message should not be modified")
	assert.Nil(t, asyncResp, "no async response expected")
}

func TestBridge_OutputEventForwarding(t *testing.T) {
	t.Parallel()

	// Test that output events are forwarded even when captured

	stdoutBuf := &bytes.Buffer{}

	config := BridgeConfig{
		SessionID: "session",
		OutputHandler: &testOutputHandler{
			stdout: stdoutBuf,
		},
	}

	bridge := NewDapBridge(config)

	outputEvent := &dap.OutputEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "event",
			},
			Event: "output",
		},
		Body: dap.OutputEventBody{
			Category: "stdout",
			Output:   "test output",
		},
	}

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	modified, forward, asyncResp := bridge.interceptDownstreamMessage(ctx, outputEvent)

	// Output event should still be forwarded to IDE
	assert.True(t, forward, "output event should be forwarded")
	assert.Equal(t, outputEvent, modified)
	assert.Nil(t, asyncResp)

	// And should have been captured
	assert.Contains(t, stdoutBuf.String(), "test output")
}

func TestBridge_TerminatedEventTracking(t *testing.T) {
	t.Parallel()

	// Test that interceptDownstreamMessage tracks TerminatedEvent
	// but does not set exit code (TerminatedEvent has no exit code)

	config := BridgeConfig{
		SessionID: "session",
	}

	bridge := NewDapBridge(config)

	// Initially terminatedEventSeen should be false
	assert.False(t, bridge.terminatedEventSeen.Load())
	assert.Nil(t, bridge.ExitCode())

	terminatedEvent := &dap.TerminatedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "event",
			},
			Event: "terminated",
		},
	}

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	modified, forward, asyncResp := bridge.interceptDownstreamMessage(ctx, terminatedEvent)

	assert.True(t, forward, "terminated event should be forwarded to IDE")
	assert.Equal(t, terminatedEvent, modified)
	assert.Nil(t, asyncResp)

	// terminatedEventSeen should now be true
	assert.True(t, bridge.terminatedEventSeen.Load(), "bridge should track that TerminatedEvent was seen")

	// ExitCode should still be nil (TerminatedEvent does not carry exit code)
	assert.Nil(t, bridge.ExitCode(), "exit code should remain nil after TerminatedEvent")
}

func TestBridge_ExitedEventTracking(t *testing.T) {
	t.Parallel()

	// Test that interceptDownstreamMessage captures exit code from ExitedEvent

	config := BridgeConfig{
		SessionID: "session",
		Logger:    logr.Discard(),
	}

	bridge := NewDapBridge(config)

	assert.Nil(t, bridge.ExitCode())
	assert.False(t, bridge.terminatedEventSeen.Load())

	exitedEvent := &dap.ExitedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "event",
			},
			Event: "exited",
		},
		Body: dap.ExitedEventBody{
			ExitCode: 42,
		},
	}

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	modified, forward, asyncResp := bridge.interceptDownstreamMessage(ctx, exitedEvent)

	assert.True(t, forward, "exited event should be forwarded to IDE")
	assert.Equal(t, exitedEvent, modified)
	assert.Nil(t, asyncResp)

	// Exit code should be captured
	require.NotNil(t, bridge.ExitCode(), "exit code should be captured from ExitedEvent")
	assert.Equal(t, int32(42), *bridge.ExitCode())

	// terminatedEventSeen should remain false (ExitedEvent is semantically different)
	assert.False(t, bridge.terminatedEventSeen.Load(),
		"ExitedEvent should not set terminatedEventSeen")
}

func TestBridge_ExitedAndTerminatedEvents(t *testing.T) {
	t.Parallel()

	// Test that both ExitedEvent and TerminatedEvent can be received gracefully.
	// Typical DAP flow: ExitedEvent (with exit code) arrives first, then TerminatedEvent.

	config := BridgeConfig{
		SessionID: "session",
		Logger:    logr.Discard(),
	}

	bridge := NewDapBridge(config)

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	// Simulate ExitedEvent arriving first
	exitedEvent := &dap.ExitedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "event",
			},
			Event: "exited",
		},
		Body: dap.ExitedEventBody{
			ExitCode: 0,
		},
	}

	_, exitedForward, _ := bridge.interceptDownstreamMessage(ctx, exitedEvent)
	assert.True(t, exitedForward)
	require.NotNil(t, bridge.ExitCode())
	assert.Equal(t, int32(0), *bridge.ExitCode())
	assert.False(t, bridge.terminatedEventSeen.Load())

	// Simulate TerminatedEvent arriving second
	terminatedEvent := &dap.TerminatedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  2,
				Type: "event",
			},
			Event: "terminated",
		},
	}

	_, termForward, _ := bridge.interceptDownstreamMessage(ctx, terminatedEvent)
	assert.True(t, termForward)
	assert.True(t, bridge.terminatedEventSeen.Load())

	// Exit code should still be the value from ExitedEvent
	require.NotNil(t, bridge.ExitCode())
	assert.Equal(t, int32(0), *bridge.ExitCode())
}

func TestBridge_ExitedEventZeroExitCode(t *testing.T) {
	t.Parallel()

	// Test that exit code 0 is properly captured (not confused with nil/unset)

	config := BridgeConfig{
		SessionID: "session",
		Logger:    logr.Discard(),
	}

	bridge := NewDapBridge(config)

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	exitedEvent := &dap.ExitedEvent{
		Event: dap.Event{
			ProtocolMessage: dap.ProtocolMessage{
				Seq:  1,
				Type: "event",
			},
			Event: "exited",
		},
		Body: dap.ExitedEventBody{
			ExitCode: 0,
		},
	}

	bridge.interceptDownstreamMessage(ctx, exitedEvent)

	require.NotNil(t, bridge.ExitCode(), "exit code 0 should be captured, not nil")
	assert.Equal(t, int32(0), *bridge.ExitCode())
}

func TestBridge_SendErrorToIDE(t *testing.T) {
	t.Parallel()

	// Test that sendErrorToIDE sends an OutputEvent followed by a TerminatedEvent
	// through the IDE transport

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	config := BridgeConfig{
		SessionID: "session",
		Logger:    logr.Discard(),
	}

	bridge := NewDapBridge(config)

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	bridge.ideTransport = NewUnixTransportWithContext(ctx, serverConn)

	// Read messages from the client side in a goroutine
	clientTransport := NewUnixTransportWithContext(ctx, clientConn)
	msgCh := make(chan dap.Message, 2)
	go func() {
		for i := 0; i < 2; i++ {
			msg, readErr := clientTransport.ReadMessage()
			if readErr != nil {
				return
			}
			msgCh <- msg
		}
	}()

	bridge.sendErrorToIDE("adapter crashed unexpectedly")

	// Should receive OutputEvent first
	msg1 := <-msgCh
	outputEvent, ok := msg1.(*dap.OutputEvent)
	require.True(t, ok, "first message should be OutputEvent, got %T", msg1)
	assert.Equal(t, "stderr", outputEvent.Body.Category)
	assert.Contains(t, outputEvent.Body.Output, "adapter crashed unexpectedly")

	// Then TerminatedEvent
	msg2 := <-msgCh
	_, ok = msg2.(*dap.TerminatedEvent)
	require.True(t, ok, "second message should be TerminatedEvent, got %T", msg2)
}

func TestBridge_SendTerminatedToIDE(t *testing.T) {
	t.Parallel()

	// Test that sendTerminatedToIDE sends only a TerminatedEvent

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	config := BridgeConfig{
		SessionID: "session",
		Logger:    logr.Discard(),
	}

	bridge := NewDapBridge(config)

	ctx, cancel := pkgtestutil.GetTestContext(t, 5*time.Second)
	defer cancel()

	bridge.ideTransport = NewUnixTransportWithContext(ctx, serverConn)

	// Read messages from the client side
	clientTransport := NewUnixTransportWithContext(ctx, clientConn)
	msgCh := make(chan dap.Message, 1)
	go func() {
		msg, readErr := clientTransport.ReadMessage()
		if readErr != nil {
			return
		}
		msgCh <- msg
	}()

	bridge.sendTerminatedToIDE()

	msg := <-msgCh
	_, ok := msg.(*dap.TerminatedEvent)
	require.True(t, ok, "message should be TerminatedEvent, got %T", msg)
}

func TestBridge_SendErrorToIDE_NilTransport(t *testing.T) {
	t.Parallel()

	// Test that sendErrorToIDE is a no-op when ideTransport is nil (no panic)

	config := BridgeConfig{
		SessionID: "session",
		Logger:    logr.Discard(),
	}

	bridge := NewDapBridge(config)

	// Should not panic
	bridge.sendErrorToIDE("some error")
	bridge.sendTerminatedToIDE()
}

// performHandshakeWithAdapterConfig sends a full handshake request including
// debug adapter configuration, and reads the response.
// This is needed because performClientHandshake does not include adapter config,
// making it insufficient for end-to-end tests through BridgeSocketManager.
func performHandshakeWithAdapterConfig(
	conn net.Conn,
	token, sessionID, runID string,
	adapterConfig *DebugAdapterConfig,
) error {
	writer := NewHandshakeWriter(conn)
	reader := NewHandshakeReader(conn)

	req := &HandshakeRequest{
		Token:              token,
		SessionID:          sessionID,
		RunID:              runID,
		DebugAdapterConfig: adapterConfig,
	}
	if writeErr := writer.WriteRequest(req); writeErr != nil {
		return fmt.Errorf("failed to send handshake request: %w", writeErr)
	}

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

// resolveDebuggeeSourcePath returns the absolute path to test/debuggee/debuggee.go.
func resolveDebuggeeSourcePath(t *testing.T) string {
	t.Helper()
	rootDir, findErr := osutil.FindRootFor(osutil.FileTarget, "test", "debuggee", "debuggee.go")
	require.NoError(t, findErr, "could not find repo root containing test/debuggee/debuggee.go")
	return filepath.Join(rootDir, "test", "debuggee", "debuggee.go")
}

func TestBridge_DelveEndToEnd(t *testing.T) {
	t.Parallel()

	// Locate the debuggee binary (built by 'make test-prereqs' with debug symbols).
	toolDir, toolDirErr := testutil.GetTestToolDir("debuggee")
	if toolDirErr != nil {
		t.Skip("debuggee binary not found (run 'make test-prereqs' first):", toolDirErr)
	}
	debuggeeName := "debuggee"
	if runtime.GOOS == "windows" {
		debuggeeName += ".exe"
	}
	debuggeeBinary := filepath.Join(toolDir, debuggeeName)

	// Resolve the source file path for setting breakpoints.
	debuggeeSource := resolveDebuggeeSourcePath(t)
	breakpointLine := 18 // result := compute(10)

	ctx, cancel := pkgtestutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	log := logr.Discard()
	executor := process.NewOSExecutor(log)
	defer executor.Dispose()

	// Set up bridge manager and register a session.
	socketDir := shortTempDir(t)
	manager := NewBridgeManager(BridgeManagerConfig{
		SocketDir:        socketDir,
		Executor:         executor,
		HandshakeTimeout: 5 * time.Second,
	}, log)

	token := "test-delve-token"
	sessionID := "delve-e2e-session"
	session, regErr := manager.RegisterSession(sessionID, token)
	require.NoError(t, regErr)
	require.NotNil(t, session)

	// Run bridge manager in background.
	go func() {
		_ = manager.Run(ctx)
	}()

	select {
	case <-manager.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("bridge manager failed to become ready")
	}

	socketPath, socketPathErr := manager.SocketPath(ctx)
	require.NoError(t, socketPathErr)
	require.NotEmpty(t, socketPath)

	// Connect to the Unix socket as the IDE.
	ideConn, dialErr := net.Dial("unix", socketPath)
	require.NoError(t, dialErr)
	defer ideConn.Close()

	// Perform handshake with dlv dap adapter config (tcp-callback: bridge listens, dlv connects).
	// The adapter process does not inherit the current process environment, so we must
	// explicitly pass environment variables needed by the Go toolchain.
	adapterEnv := envVarsFromOS("PATH", "HOME", "GOPATH", "GOROOT", "GOMODCACHE")
	handshakeErr := performHandshakeWithAdapterConfig(ideConn, token, sessionID, "delve-run-id", &DebugAdapterConfig{
		Args: []string{
			"go", "tool", "github.com/go-delve/delve/cmd/dlv",
			"dap", "--client-addr=127.0.0.1:{{port}}",
		},
		Mode: DebugAdapterModeTCPCallback,
		Env:  adapterEnv,
	})
	require.NoError(t, handshakeErr, "handshake with adapter config should succeed")

	// Create the DAP test client over the connected Unix socket.
	ideTransport := NewUnixTransportWithContext(ctx, ideConn)
	client := NewTestClient(ctx, ideTransport)
	defer client.Close()

	// === DAP Protocol Sequence ===
	// dlv sends the 'initialized' event after receiving the 'launch' request,
	// so the sequence is: initialize → launch → initialized → setBreakpoints → configurationDone.

	// 1. Initialize
	initResp, initErr := client.Initialize(ctx)
	require.NoError(t, initErr, "initialize should succeed")
	require.NotNil(t, initResp)
	assert.True(t, initResp.Body.SupportsConfigurationDoneRequest,
		"dlv should support configurationDone")

	// 2. Launch the debuggee binary (exec mode — dlv runs the pre-built binary directly).
	launchErr := client.Launch(ctx, debuggeeBinary, false)
	require.NoError(t, launchErr, "launch should succeed")

	// 3. Wait for the 'initialized' event from dlv (sent after launch).
	_, initializedErr := client.WaitForEvent("initialized", 10*time.Second)
	require.NoError(t, initializedErr, "should receive initialized event from dlv")

	// 4. Set breakpoints on the debuggee source.
	bpResp, bpErr := client.SetBreakpoints(ctx, debuggeeSource, []int{breakpointLine})
	require.NoError(t, bpErr, "setBreakpoints should succeed")
	require.Len(t, bpResp.Body.Breakpoints, 1)
	assert.True(t, bpResp.Body.Breakpoints[0].Verified,
		"breakpoint at line %d should be verified", breakpointLine)

	// 5. Signal configuration is complete — program begins executing.
	configDoneErr := client.ConfigurationDone(ctx)
	require.NoError(t, configDoneErr, "configurationDone should succeed")

	// 6. Wait for the program to hit the breakpoint.
	stoppedEvent, stoppedErr := client.WaitForStoppedEvent(10 * time.Second)
	require.NoError(t, stoppedErr, "should receive stopped event at breakpoint")
	assert.Equal(t, "breakpoint", stoppedEvent.Body.Reason)
	assert.Greater(t, stoppedEvent.Body.ThreadId, 0, "thread ID should be positive")

	// 7. Continue execution — program runs to completion.
	continueErr := client.Continue(ctx, stoppedEvent.Body.ThreadId)
	require.NoError(t, continueErr, "continue should succeed")

	// 8. Wait for the program to terminate.
	terminatedErr := client.WaitForTerminatedEvent(10 * time.Second)
	require.NoError(t, terminatedErr, "should receive terminated event")

	// 9. Disconnect from the debug adapter.
	disconnectErr := client.Disconnect(ctx, true)
	require.NoError(t, disconnectErr, "disconnect should succeed")
}

// envVarsFromOS returns apiv1.EnvVar entries for the given environment variable names,
// including only those that are set in the current process environment.
func envVarsFromOS(names ...string) []apiv1.EnvVar {
	var envVars []apiv1.EnvVar
	for _, name := range names {
		if val, ok := os.LookupEnv(name); ok {
			envVars = append(envVars, apiv1.EnvVar{Name: name, Value: val})
		}
	}
	return envVars
}
