//go:build integration

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-dap"
	"github.com/microsoft/dcp/internal/dap/proto"
	"github.com/microsoft/dcp/pkg/commonapi"
	"github.com/microsoft/dcp/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// waitPollInterval is the interval between polling attempts in wait functions.
	waitPollInterval = 10 * time.Millisecond
	// pollImmediately indicates whether to poll immediately before waiting.
	pollImmediately = true
)

// delveInstance represents a running Delve DAP server.
type delveInstance struct {
	cmd     *exec.Cmd
	addr    string
	cancel  context.CancelFunc
	done    chan error
	cleanup func()
}

// startDelve starts a Delve DAP server and returns its address.
// The caller must call cleanup() when done.
func startDelve(ctx context.Context, t *testing.T) (*delveInstance, error) {
	t.Helper()

	// Create a cancellable context for the Delve process
	delveCtx, cancel := context.WithCancel(ctx)

	// Start Delve in DAP mode
	// Use go tool dlv since we have it as a tool dependency
	cmd := exec.CommandContext(delveCtx, "go", "tool", "dlv", "dap", "-l", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "GOFLAGS=") // Clear GOFLAGS to avoid issues

	// Capture stdout to parse the listening address (Delve prints to stdout)
	stdout, stdoutPipeErr := cmd.StdoutPipe()
	if stdoutPipeErr != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", stdoutPipeErr)
	}

	// Also capture stderr for debugging
	cmd.Stderr = os.Stderr

	if startErr := cmd.Start(); startErr != nil {
		cancel()
		return nil, fmt.Errorf("failed to start delve: %w", startErr)
	}

	t.Logf("Started Delve process with PID %d", cmd.Process.Pid)

	// Channel to signal when Delve exits
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	// Parse stdout to find the listening address
	// Delve prints: "DAP server listening at: 127.0.0.1:XXXXX"
	addrChan := make(chan string, 1)
	errChan := make(chan error, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		addrRegex := regexp.MustCompile(`DAP server listening at:\s*(\S+)`)

		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("Delve: %s", line)

			if matches := addrRegex.FindStringSubmatch(line); len(matches) > 1 {
				addrChan <- matches[1]
				return
			}
		}

		if scanErr := scanner.Err(); scanErr != nil {
			errChan <- fmt.Errorf("error reading delve stdout: %w", scanErr)
		} else {
			errChan <- fmt.Errorf("delve exited without printing address")
		}
	}()

	// Wait for address or timeout
	select {
	case addr := <-addrChan:
		t.Logf("Delve DAP server listening at: %s", addr)

		cleanup := func() {
			cancel()
			// Give Delve time to shutdown gracefully
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}

		return &delveInstance{
			cmd:     cmd,
			addr:    addr,
			cancel:  cancel,
			done:    done,
			cleanup: cleanup,
		}, nil

	case parseErr := <-errChan:
		cancel()
		return nil, parseErr

	case <-time.After(10 * time.Second):
		cancel()
		return nil, fmt.Errorf("timeout waiting for delve to start")

	case waitErr := <-done:
		cancel()
		return nil, fmt.Errorf("delve exited unexpectedly: %w", waitErr)
	}
}

// getDebuggeeDir returns the directory containing the debuggee source.
func getDebuggeeDir(t *testing.T) string {
	t.Helper()

	// Find the repository root by looking for go.mod
	dir, lookErr := os.Getwd()
	if lookErr != nil {
		t.Fatalf("Failed to get working directory: %v", lookErr)
	}

	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return filepath.Join(dir, "test", "debuggee")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("Could not find repository root")
		}
		dir = parent
	}
}

// getDebuggeeBinary returns the path to the compiled debuggee binary.
func getDebuggeeBinary(t *testing.T) string {
	t.Helper()

	// Find the repository root by looking for go.mod
	dir, lookErr := os.Getwd()
	if lookErr != nil {
		t.Fatalf("Failed to get working directory: %v", lookErr)
	}

	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			binary := filepath.Join(dir, ".toolbin", "debuggee")
			if _, statErr := os.Stat(binary); statErr != nil {
				t.Fatalf("Debuggee binary not found at %s. Run 'make test-prereqs' first.", binary)
			}
			return binary
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("Could not find repository root")
		}
		dir = parent
	}
}

// TestProxy_E2E_DelveDebugSession tests a complete debug session through the proxy.
func TestProxy_E2E_DelveDebugSession(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 60*time.Second)
	defer cancel()

	// Start Delve
	delve, startErr := startDelve(ctx, t)
	if startErr != nil {
		t.Fatalf("Failed to start Delve: %v", startErr)
	}
	defer delve.cleanup()

	// Create a TCP listener for the proxy's upstream (client-facing) side
	upstreamListener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("Failed to create upstream listener: %v", listenErr)
	}
	defer upstreamListener.Close()
	t.Logf("Proxy upstream listening at: %s", upstreamListener.Addr().String())

	// Connect to Delve (proxy downstream)
	downstreamConn, dialErr := net.Dial("tcp", delve.addr)
	if dialErr != nil {
		t.Fatalf("Failed to connect to Delve: %v", dialErr)
	}
	downstreamTransport := NewTCPTransport(downstreamConn)

	// Accept client connection in background
	var upstreamConn net.Conn
	var acceptErr error
	var acceptWg sync.WaitGroup
	acceptWg.Add(1)
	go func() {
		defer acceptWg.Done()
		upstreamConn, acceptErr = upstreamListener.Accept()
	}()

	// Connect test client to proxy
	clientConn, clientDialErr := net.Dial("tcp", upstreamListener.Addr().String())
	if clientDialErr != nil {
		t.Fatalf("Failed to connect client to proxy: %v", clientDialErr)
	}
	clientTransport := NewTCPTransport(clientConn)

	// Wait for accept
	acceptWg.Wait()
	if acceptErr != nil {
		t.Fatalf("Failed to accept client connection: %v", acceptErr)
	}
	upstreamTransport := NewTCPTransport(upstreamConn)

	// Create and start the proxy with a test logger
	testLog := testutil.NewLogForTesting("dap-proxy")
	proxy := NewProxy(upstreamTransport, downstreamTransport, ProxyConfig{
		Logger: testLog,
	})

	var proxyWg sync.WaitGroup
	proxyWg.Add(1)
	go func() {
		defer proxyWg.Done()
		proxyErr := proxy.Start(ctx)
		if proxyErr != nil && ctx.Err() == nil {
			t.Logf("Proxy error: %v", proxyErr)
		}
	}()

	// Create test client
	client := NewTestClient(clientTransport)
	defer client.Close()

	// Get debuggee paths
	debuggeeDir := getDebuggeeDir(t)
	debuggeeBinary := getDebuggeeBinary(t)
	debuggeeSource := filepath.Join(debuggeeDir, "debuggee.go")

	t.Logf("Debuggee binary: %s", debuggeeBinary)
	t.Logf("Debuggee source: %s", debuggeeSource)

	// === Debug Session Flow ===

	// 1. Initialize
	t.Log("Sending initialize request...")
	initResp, initErr := client.Initialize(ctx)
	if initErr != nil {
		t.Fatalf("Initialize failed: %v", initErr)
	}
	t.Logf("Initialize response: supportsConfigurationDoneRequest=%v", initResp.Body.SupportsConfigurationDoneRequest)

	// Wait for initialized event
	// Note: Some adapters send initialized immediately, some after launch
	t.Log("Waiting for initialized event...")
	_, initEvtErr := client.WaitForEvent("initialized", 2*time.Second)
	if initEvtErr != nil {
		t.Log("No initialized event received (may come after launch)")
	} else {
		t.Log("Received initialized event")
	}

	// 2. Launch
	t.Log("Sending launch request...")
	launchErr := client.Launch(ctx, debuggeeBinary, false)
	if launchErr != nil {
		t.Fatalf("Launch failed: %v", launchErr)
	}
	t.Log("Launch successful")

	// 3. Set breakpoints
	t.Log("Setting breakpoints...")
	bpResp, bpErr := client.SetBreakpoints(ctx, debuggeeSource, []int{18}) // Line with compute() call
	if bpErr != nil {
		t.Fatalf("SetBreakpoints failed: %v", bpErr)
	}
	if len(bpResp.Body.Breakpoints) == 0 {
		t.Fatal("No breakpoints returned")
	}
	t.Logf("Breakpoint set: verified=%v, line=%d", bpResp.Body.Breakpoints[0].Verified, bpResp.Body.Breakpoints[0].Line)

	// 4. Configuration done
	t.Log("Sending configurationDone...")
	configErr := client.ConfigurationDone(ctx)
	if configErr != nil {
		t.Fatalf("ConfigurationDone failed: %v", configErr)
	}
	t.Log("ConfigurationDone successful")

	// 5. Wait for stopped event (hit breakpoint)
	t.Log("Waiting for stopped event...")
	stoppedEvent, stoppedErr := client.WaitForStoppedEvent(10 * time.Second)
	if stoppedErr != nil {
		t.Fatalf("Failed to receive stopped event: %v", stoppedErr)
	}
	t.Logf("Stopped at: reason=%s, threadId=%d", stoppedEvent.Body.Reason, stoppedEvent.Body.ThreadId)

	// Verify we stopped at a breakpoint
	if !strings.Contains(stoppedEvent.Body.Reason, "breakpoint") {
		t.Errorf("Expected stopped reason to contain 'breakpoint', got: %s", stoppedEvent.Body.Reason)
	}

	// 6. Continue execution
	t.Log("Sending continue request...")
	contErr := client.Continue(ctx, stoppedEvent.Body.ThreadId)
	if contErr != nil {
		t.Fatalf("Continue failed: %v", contErr)
	}
	t.Log("Continue successful")

	// 7. Wait for terminated event (program finished)
	t.Log("Waiting for terminated event...")
	termErr := client.WaitForTerminatedEvent(10 * time.Second)
	if termErr != nil {
		t.Fatalf("Failed to receive terminated event: %v", termErr)
	}
	t.Log("Received terminated event")

	// 8. Disconnect (use a short timeout since the adapter may close the connection)
	t.Log("Sending disconnect request...")
	disconnCtx, disconnCancel := context.WithTimeout(ctx, 2*time.Second)
	disconnErr := client.Disconnect(disconnCtx, false)
	disconnCancel()
	if disconnErr != nil {
		t.Logf("Disconnect error (may be expected): %v", disconnErr)
	} else {
		t.Log("Disconnect successful")
	}

	// Cleanup
	proxy.Stop()
	proxyWg.Wait()

	t.Log("End-to-end test completed successfully!")
}

// TestGRPC_E2E_ControlServerWithDelve tests the gRPC control service with a live Delve session.
// This test verifies:
// - Session establishment and handshake
// - Virtual requests sent from the control server
// - Event forwarding from proxy to server
// - Session status updates
// - Session termination
func TestGRPC_E2E_ControlServerWithDelve(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 60*time.Second)
	defer cancel()

	// Start Delve
	delve, startErr := startDelve(ctx, t)
	if startErr != nil {
		t.Fatalf("Failed to start Delve: %v", startErr)
	}
	defer delve.cleanup()

	// === Setup gRPC Control Server ===
	grpcListener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("Failed to create gRPC listener: %v", listenErr)
	}
	t.Logf("gRPC server listening at: %s", grpcListener.Addr().String())

	// Track received events
	var eventsReceived atomic.Int32
	var lastEventPayload []byte
	var eventMu sync.Mutex

	testLog := testutil.NewLogForTesting("grpc-server")
	server := NewControlServer(ControlServerConfig{
		Listener:    grpcListener,
		BearerToken: "test-token",
		Logger:      testLog,
		EventHandler: func(key commonapi.NamespacedNameWithKind, payload []byte) {
			eventsReceived.Add(1)
			eventMu.Lock()
			lastEventPayload = payload
			eventMu.Unlock()
			t.Logf("Received event from %s: %d bytes", key.String(), len(payload))
		},
		RunInTerminalHandler: func(ctx context.Context, key commonapi.NamespacedNameWithKind, req *proto.RunInTerminalRequest) *proto.RunInTerminalResponse {
			t.Logf("Received RunInTerminal request: kind=%s, title=%s", req.GetKind(), req.GetTitle())
			return &proto.RunInTerminalResponse{
				ProcessId:      ptrInt64(12345),
				ShellProcessId: ptrInt64(12346),
			}
		},
	})

	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		if serverErr := server.Start(ctx); serverErr != nil && ctx.Err() == nil {
			t.Logf("Server error: %v", serverErr)
		}
	}()
	defer func() {
		server.Stop()
		serverWg.Wait()
	}()

	// Wait for gRPC server to be ready to accept connections
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		conn, dialErr := net.DialTimeout("tcp", grpcListener.Addr().String(), 50*time.Millisecond)
		if dialErr != nil {
			return false, nil // Keep polling
		}
		conn.Close()
		return true, nil
	})
	require.NoError(t, waitErr, "gRPC server should be ready")

	// === Setup Proxy with Session Driver ===
	resourceKey := commonapi.NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: "test-namespace",
			Name:      "test-debuggee",
		},
		Kind: schema.GroupVersionKind{
			Group:   "dcp.io",
			Version: "v1",
			Kind:    "Executable",
		},
	}

	// Create a TCP listener for the proxy's upstream (client-facing) side
	upstreamListener, upListenErr := net.Listen("tcp", "127.0.0.1:0")
	if upListenErr != nil {
		t.Fatalf("Failed to create upstream listener: %v", upListenErr)
	}
	defer upstreamListener.Close()
	t.Logf("Proxy upstream listening at: %s", upstreamListener.Addr().String())

	// Connect to Delve (proxy downstream)
	downstreamConn, dialErr := net.Dial("tcp", delve.addr)
	if dialErr != nil {
		t.Fatalf("Failed to connect to Delve: %v", dialErr)
	}
	downstreamTransport := NewTCPTransport(downstreamConn)

	// Accept client connection in background
	var upstreamConn net.Conn
	var acceptErr error
	var acceptWg sync.WaitGroup
	acceptWg.Add(1)
	go func() {
		defer acceptWg.Done()
		upstreamConn, acceptErr = upstreamListener.Accept()
	}()

	// Connect test client to proxy
	clientConn, clientDialErr := net.Dial("tcp", upstreamListener.Addr().String())
	if clientDialErr != nil {
		t.Fatalf("Failed to connect client to proxy: %v", clientDialErr)
	}
	clientTransport := NewTCPTransport(clientConn)

	// Wait for accept
	acceptWg.Wait()
	if acceptErr != nil {
		t.Fatalf("Failed to accept client connection: %v", acceptErr)
	}
	upstreamTransport := NewTCPTransport(upstreamConn)

	// Create proxy
	proxyLog := testutil.NewLogForTesting("dap-proxy")
	proxy := NewProxy(upstreamTransport, downstreamTransport, ProxyConfig{
		Logger: proxyLog,
	})

	// Create control client
	clientLog := testutil.NewLogForTesting("grpc-client")
	controlClient := NewControlClient(ControlClientConfig{
		Endpoint:    grpcListener.Addr().String(),
		BearerToken: "test-token",
		ResourceKey: resourceKey,
		Logger:      clientLog,
	})

	// Create session driver
	driverLog := testutil.NewLogForTesting("session-driver")
	driver := NewSessionDriver(proxy, controlClient, driverLog)

	// Start session driver
	var driverWg sync.WaitGroup
	driverWg.Add(1)
	go func() {
		defer driverWg.Done()
		if driverErr := driver.Run(ctx); driverErr != nil {
			t.Logf("Session driver error: %v", driverErr)
		}
	}()

	// === Verify Session Registered ===
	t.Log("Verifying session registration...")
	var sessionState *DebugSessionState
	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		sessionState = server.GetSessionStatus(resourceKey)
		return sessionState != nil, nil
	})
	require.NoError(t, waitErr, "Session should be registered")
	t.Logf("Session registered with status: %s", sessionState.Status.String())

	// === Run Debug Session Flow via Test Client ===
	testClient := NewTestClient(clientTransport)
	defer testClient.Close()

	debuggeeDir := getDebuggeeDir(t)
	debuggeeBinary := getDebuggeeBinary(t)
	debuggeeSource := filepath.Join(debuggeeDir, "debuggee.go")

	t.Logf("Debuggee binary: %s", debuggeeBinary)

	// 1. Initialize
	t.Log("Sending initialize request...")
	initResp, initErr := testClient.Initialize(ctx)
	require.NoError(t, initErr, "Initialize should succeed")
	t.Logf("Initialize response: supportsConfigurationDoneRequest=%v", initResp.Body.SupportsConfigurationDoneRequest)

	// Wait for initialized event
	_, _ = testClient.WaitForEvent("initialized", 2*time.Second)

	// 2. Launch
	t.Log("Sending launch request...")
	launchErr := testClient.Launch(ctx, debuggeeBinary, false)
	require.NoError(t, launchErr, "Launch should succeed")
	t.Log("Launch successful")

	// 3. Set breakpoints
	t.Log("Setting breakpoints...")
	bpResp, bpErr := testClient.SetBreakpoints(ctx, debuggeeSource, []int{18})
	require.NoError(t, bpErr, "SetBreakpoints should succeed")
	require.NotEmpty(t, bpResp.Body.Breakpoints, "Should have breakpoints")
	t.Logf("Breakpoint set: verified=%v, line=%d", bpResp.Body.Breakpoints[0].Verified, bpResp.Body.Breakpoints[0].Line)

	// 4. Configuration done
	t.Log("Sending configurationDone...")
	configErr := testClient.ConfigurationDone(ctx)
	require.NoError(t, configErr, "ConfigurationDone should succeed")
	t.Log("ConfigurationDone successful")

	// Note: We don't check status immediately after configurationDone because
	// Delve may have already hit the breakpoint and transitioned to "stopped".
	// The status transition is:  connecting -> initializing -> attached -> stopped

	// 5. Wait for stopped event (hit breakpoint)
	t.Log("Waiting for stopped event...")
	stoppedEvent, stoppedErr := testClient.WaitForStoppedEvent(10 * time.Second)
	require.NoError(t, stoppedErr, "Should receive stopped event")
	t.Logf("Stopped at: reason=%s, threadId=%d", stoppedEvent.Body.Reason, stoppedEvent.Body.ThreadId)
	assert.Contains(t, stoppedEvent.Body.Reason, "breakpoint")

	// Wait for status to reach "stopped"
	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		sessionState = server.GetSessionStatus(resourceKey)
		return sessionState != nil && sessionState.Status == DebugSessionStatusStopped, nil
	})
	require.NoError(t, waitErr, "Session status should be stopped")
	t.Logf("Session status: %s", sessionState.Status.String())

	// === Test Virtual Request: Threads ===
	t.Log("Sending virtual threads request...")
	threadsReq := &dap.ThreadsRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "request",
			},
			Command: "threads",
		},
	}
	threadsPayload, _ := json.Marshal(threadsReq)

	threadsRespPayload, virtualErr := server.SendVirtualRequest(ctx, resourceKey, threadsPayload, 5*time.Second)
	require.NoError(t, virtualErr, "Virtual request should succeed")
	require.NotEmpty(t, threadsRespPayload, "Should have response payload")

	// Parse response
	var threadsResp dap.ThreadsResponse
	parseErr := json.Unmarshal(threadsRespPayload, &threadsResp)
	require.NoError(t, parseErr, "Should parse threads response")
	assert.True(t, threadsResp.Response.Success, "Threads request should succeed")
	assert.NotEmpty(t, threadsResp.Body.Threads, "Should have threads")
	t.Logf("Virtual threads request returned %d threads", len(threadsResp.Body.Threads))

	// === Test Virtual Request: Stack Trace ===
	t.Log("Sending virtual stackTrace request...")
	stackReq := &dap.StackTraceRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "request",
			},
			Command: "stackTrace",
		},
		Arguments: dap.StackTraceArguments{
			ThreadId: stoppedEvent.Body.ThreadId,
		},
	}
	stackPayload, _ := json.Marshal(stackReq)

	stackRespPayload, stackVirtualErr := server.SendVirtualRequest(ctx, resourceKey, stackPayload, 5*time.Second)
	require.NoError(t, stackVirtualErr, "Virtual stack request should succeed")

	var stackResp dap.StackTraceResponse
	stackParseErr := json.Unmarshal(stackRespPayload, &stackResp)
	require.NoError(t, stackParseErr, "Should parse stack response")
	assert.True(t, stackResp.Response.Success, "StackTrace request should succeed")
	assert.NotEmpty(t, stackResp.Body.StackFrames, "Should have stack frames")
	t.Logf("Virtual stackTrace request returned %d frames", len(stackResp.Body.StackFrames))

	// === Verify Events Were Received ===
	t.Logf("Total events received by server: %d", eventsReceived.Load())
	assert.Greater(t, eventsReceived.Load(), int32(0), "Should have received events")

	// Examine last event
	eventMu.Lock()
	if lastEventPayload != nil {
		var eventBase struct {
			Type  string `json:"type"`
			Event string `json:"event,omitempty"`
		}
		if err := json.Unmarshal(lastEventPayload, &eventBase); err == nil {
			t.Logf("Last event type: %s, event: %s", eventBase.Type, eventBase.Event)
		}
	}
	eventMu.Unlock()

	// 6. Continue execution
	t.Log("Sending continue request...")
	contErr := testClient.Continue(ctx, stoppedEvent.Body.ThreadId)
	require.NoError(t, contErr, "Continue should succeed")
	t.Log("Continue successful")

	// 7. Wait for terminated event
	t.Log("Waiting for terminated event...")
	termEvtErr := testClient.WaitForTerminatedEvent(10 * time.Second)
	require.NoError(t, termEvtErr, "Should receive terminated event")
	t.Log("Received terminated event")

	// Wait for status to reach "terminated"
	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		sessionState = server.GetSessionStatus(resourceKey)
		return sessionState != nil && sessionState.Status == DebugSessionStatusTerminated, nil
	})
	require.NoError(t, waitErr, "Session status should be terminated")
	t.Logf("Final session status: %s", sessionState.Status.String())

	// 8. Disconnect
	t.Log("Sending disconnect request...")
	disconnCtx, disconnCancel := context.WithTimeout(ctx, 2*time.Second)
	disconnErr := testClient.Disconnect(disconnCtx, false)
	disconnCancel()
	if disconnErr != nil {
		t.Logf("Disconnect error (may be expected): %v", disconnErr)
	}

	// === Test Session Termination from Server ===
	// This happens when the controller stops/deletes the resource
	t.Log("Terminating session from server...")
	server.TerminateSession(resourceKey, "test termination")

	// Wait for driver to complete (termination signal propagates)
	driverDone := make(chan struct{})
	go func() {
		driverWg.Wait()
		close(driverDone)
	}()

	select {
	case <-driverDone:
		t.Log("Driver completed after termination")
	case <-time.After(5 * time.Second):
		t.Log("Driver did not complete within timeout (continuing)")
	}

	// Verify session status after termination
	sessionState = server.GetSessionStatus(resourceKey)
	if sessionState != nil {
		t.Logf("Session status after termination: %s", sessionState.Status.String())
	}

	// Cleanup
	cancel()

	t.Log("gRPC integration test completed successfully!")
}

// TestGRPC_E2E_VirtualRequestTimeout tests that virtual requests respect their timeout.
func TestGRPC_E2E_VirtualRequestTimeout(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	// Start Delve
	delve, startErr := startDelve(ctx, t)
	if startErr != nil {
		t.Fatalf("Failed to start Delve: %v", startErr)
	}
	defer delve.cleanup()

	// Setup gRPC server
	grpcListener, _ := net.Listen("tcp", "127.0.0.1:0")
	testLog := testutil.NewLogForTesting("grpc-server")
	server := NewControlServer(ControlServerConfig{
		Listener:    grpcListener,
		BearerToken: "test-token",
		Logger:      testLog,
	})

	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		_ = server.Start(ctx)
	}()
	defer func() {
		server.Stop()
		serverWg.Wait()
	}()

	// Wait for gRPC server to be ready
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		conn, dialErr := net.DialTimeout("tcp", grpcListener.Addr().String(), 50*time.Millisecond)
		if dialErr != nil {
			return false, nil
		}
		conn.Close()
		return true, nil
	})
	require.NoError(t, waitErr, "gRPC server should be ready")

	// Setup proxy with session driver
	resourceKey := commonapi.NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: "test-ns",
			Name:      "timeout-test",
		},
		Kind: schema.GroupVersionKind{
			Group:   "dcp.io",
			Version: "v1",
			Kind:    "Executable",
		},
	}

	// Connect proxy to Delve
	upstreamListener, _ := net.Listen("tcp", "127.0.0.1:0")
	defer upstreamListener.Close()

	downstreamConn, dialErr := net.Dial("tcp", delve.addr)
	if dialErr != nil {
		t.Fatalf("Failed to connect to Delve: %v", dialErr)
	}
	downstreamTransport := NewTCPTransport(downstreamConn)

	var upstreamConn net.Conn
	var acceptWg sync.WaitGroup
	acceptWg.Add(1)
	go func() {
		defer acceptWg.Done()
		upstreamConn, _ = upstreamListener.Accept()
	}()

	clientConn, _ := net.Dial("tcp", upstreamListener.Addr().String())
	clientTransport := NewTCPTransport(clientConn)
	testClient := NewTestClient(clientTransport)
	defer testClient.Close()

	acceptWg.Wait()
	upstreamTransport := NewTCPTransport(upstreamConn)

	proxy := NewProxy(upstreamTransport, downstreamTransport, ProxyConfig{
		Logger: testutil.NewLogForTesting("proxy"),
	})

	controlClient := NewControlClient(ControlClientConfig{
		Endpoint:    grpcListener.Addr().String(),
		BearerToken: "test-token",
		ResourceKey: resourceKey,
		Logger:      testutil.NewLogForTesting("client"),
	})

	driver := NewSessionDriver(proxy, controlClient, testutil.NewLogForTesting("driver"))

	var driverWg sync.WaitGroup
	driverWg.Add(1)
	go func() {
		defer driverWg.Done()
		_ = driver.Run(ctx)
	}()

	// Wait for session to be registered
	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return server.GetSessionStatus(resourceKey) != nil, nil
	})
	require.NoError(t, waitErr, "Session should be registered")

	// Initialize but don't launch - this means some requests will hang
	t.Log("Initializing debug session...")
	_, initErr := testClient.Initialize(ctx)
	require.NoError(t, initErr)

	// Now send a virtual request with a short timeout before the adapter is ready
	// (we haven't launched so evaluate won't work)
	t.Log("Sending virtual request with short timeout...")
	evalReq := &dap.EvaluateRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Type: "request"},
			Command:         "evaluate",
		},
		Arguments: dap.EvaluateArguments{
			Expression: "1+1",
		},
	}
	evalPayload, _ := json.Marshal(evalReq)

	// Use a very short timeout - the evaluate should fail or timeout
	_, virtualErr := server.SendVirtualRequest(ctx, resourceKey, evalPayload, 500*time.Millisecond)
	if virtualErr != nil {
		t.Logf("Virtual request error (expected): %v", virtualErr)
		// Either timeout or error is acceptable here
		assert.True(t,
			strings.Contains(virtualErr.Error(), "timeout") ||
				strings.Contains(virtualErr.Error(), "failed") ||
				strings.Contains(virtualErr.Error(), "context"),
			"Error should indicate timeout or failure")
	}

	// Cleanup
	cancel()
	driverWg.Wait()

	t.Log("Timeout test completed!")
}

// TestGRPC_E2E_VirtualContinueRequest tests that a virtual Continue request from the server
// resumes debugging and the test client receives a Continued event.
func TestGRPC_E2E_VirtualContinueRequest(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 60*time.Second)
	defer cancel()

	// Start Delve
	delve, startErr := startDelve(ctx, t)
	if startErr != nil {
		t.Fatalf("Failed to start Delve: %v", startErr)
	}
	defer delve.cleanup()

	// Setup gRPC server
	grpcListener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("Failed to create gRPC listener: %v", listenErr)
	}

	testLog := testutil.NewLogForTesting("grpc-server")
	server := NewControlServer(ControlServerConfig{
		Listener:    grpcListener,
		BearerToken: "test-token",
		Logger:      testLog,
	})

	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		_ = server.Start(ctx)
	}()
	defer func() {
		server.Stop()
		serverWg.Wait()
	}()

	// Wait for gRPC server to be ready
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		conn, dialErr := net.DialTimeout("tcp", grpcListener.Addr().String(), 50*time.Millisecond)
		if dialErr != nil {
			return false, nil
		}
		conn.Close()
		return true, nil
	})
	require.NoError(t, waitErr, "gRPC server should be ready")

	// Setup resource key
	resourceKey := commonapi.NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: "test-ns",
			Name:      "virtual-continue-test",
		},
		Kind: schema.GroupVersionKind{
			Group:   "dcp.io",
			Version: "v1",
			Kind:    "Executable",
		},
	}

	// Setup proxy infrastructure
	upstreamListener, upListenErr := net.Listen("tcp", "127.0.0.1:0")
	if upListenErr != nil {
		t.Fatalf("Failed to create upstream listener: %v", upListenErr)
	}
	defer upstreamListener.Close()

	downstreamConn, dialErr := net.Dial("tcp", delve.addr)
	if dialErr != nil {
		t.Fatalf("Failed to connect to Delve: %v", dialErr)
	}
	downstreamTransport := NewTCPTransport(downstreamConn)

	var upstreamConn net.Conn
	var acceptWg sync.WaitGroup
	acceptWg.Add(1)
	go func() {
		defer acceptWg.Done()
		upstreamConn, _ = upstreamListener.Accept()
	}()

	clientConn, clientDialErr := net.Dial("tcp", upstreamListener.Addr().String())
	if clientDialErr != nil {
		t.Fatalf("Failed to connect client: %v", clientDialErr)
	}
	clientTransport := NewTCPTransport(clientConn)
	testClient := NewTestClient(clientTransport)
	defer testClient.Close()

	acceptWg.Wait()
	upstreamTransport := NewTCPTransport(upstreamConn)

	proxy := NewProxy(upstreamTransport, downstreamTransport, ProxyConfig{
		Logger: testutil.NewLogForTesting("proxy"),
	})

	controlClient := NewControlClient(ControlClientConfig{
		Endpoint:    grpcListener.Addr().String(),
		BearerToken: "test-token",
		ResourceKey: resourceKey,
		Logger:      testutil.NewLogForTesting("client"),
	})

	driver := NewSessionDriver(proxy, controlClient, testutil.NewLogForTesting("driver"))

	var driverWg sync.WaitGroup
	driverWg.Add(1)
	go func() {
		defer driverWg.Done()
		_ = driver.Run(ctx)
	}()

	// Wait for session to be registered
	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return server.GetSessionStatus(resourceKey) != nil, nil
	})
	require.NoError(t, waitErr, "Session should be registered")

	// Get debuggee paths
	debuggeeDir := getDebuggeeDir(t)
	debuggeeBinary := getDebuggeeBinary(t)
	debuggeeSource := filepath.Join(debuggeeDir, "debuggee.go")

	// === Initialize debug session ===
	t.Log("Initializing debug session...")
	_, initErr := testClient.Initialize(ctx)
	require.NoError(t, initErr, "Initialize should succeed")

	_, _ = testClient.WaitForEvent("initialized", 2*time.Second)

	// Launch
	t.Log("Launching debuggee...")
	launchErr := testClient.Launch(ctx, debuggeeBinary, false)
	require.NoError(t, launchErr, "Launch should succeed")

	// Set two breakpoints: line 18 (compute call) and line 26 (inside loop)
	// This ensures we hit the second breakpoint after continuing from the first
	t.Log("Setting breakpoints on lines 18 and 26...")
	bpResp, bpErr := testClient.SetBreakpoints(ctx, debuggeeSource, []int{18, 26})
	require.NoError(t, bpErr, "SetBreakpoints should succeed")
	require.Len(t, bpResp.Body.Breakpoints, 2, "Should have two breakpoints")
	t.Logf("Breakpoint 1: verified=%v, line=%d", bpResp.Body.Breakpoints[0].Verified, bpResp.Body.Breakpoints[0].Line)
	t.Logf("Breakpoint 2: verified=%v, line=%d", bpResp.Body.Breakpoints[1].Verified, bpResp.Body.Breakpoints[1].Line)

	// Configuration done
	t.Log("Sending configurationDone...")
	configErr := testClient.ConfigurationDone(ctx)
	require.NoError(t, configErr, "ConfigurationDone should succeed")

	// Wait for first stopped event (hit first breakpoint at line 18)
	t.Log("Waiting for first stopped event...")
	stoppedEvent, stoppedErr := testClient.WaitForStoppedEvent(10 * time.Second)
	require.NoError(t, stoppedErr, "Should receive stopped event")
	t.Logf("Stopped at first breakpoint: threadId=%d", stoppedEvent.Body.ThreadId)

	// === Send virtual Continue request from server ===
	t.Log("Sending virtual Continue request from server...")
	continueReq := &dap.ContinueRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "request",
			},
			Command: "continue",
		},
		Arguments: dap.ContinueArguments{
			ThreadId: stoppedEvent.Body.ThreadId,
		},
	}
	continuePayload, marshalErr := json.Marshal(continueReq)
	require.NoError(t, marshalErr, "Should marshal continue request")

	continueRespPayload, virtualErr := server.SendVirtualRequest(ctx, resourceKey, continuePayload, 5*time.Second)
	require.NoError(t, virtualErr, "Virtual continue request should succeed")

	// Parse and verify response
	var continueResp dap.ContinueResponse
	parseErr := json.Unmarshal(continueRespPayload, &continueResp)
	require.NoError(t, parseErr, "Should parse continue response")
	assert.True(t, continueResp.Response.Success, "Continue request should succeed")
	t.Logf("Virtual Continue response: success=%v, allThreadsContinued=%v",
		continueResp.Response.Success, continueResp.Body.AllThreadsContinued)

	// === Collect and validate event ordering ===
	// After a virtual Continue request, the proxy should generate a synthetic ContinuedEvent
	// followed by the StoppedEvent from hitting the next breakpoint
	t.Log("Collecting events until stopped...")
	events, collectErr := testClient.CollectEventsUntil("stopped", 10*time.Second)
	require.NoError(t, collectErr, "Should receive stopped event")
	require.NotEmpty(t, events, "Should have collected events")

	// Log all collected events
	t.Logf("Collected %d events:", len(events))
	var continuedEventIndex = -1
	var stoppedEventIndex = -1
	for i, evt := range events {
		if eventMsg, ok := evt.(dap.EventMessage); ok {
			eventName := eventMsg.GetEvent().Event
			t.Logf("  Event %d: %s", i, eventName)
			if eventName == "continued" {
				continuedEventIndex = i
			}
			if eventName == "stopped" {
				stoppedEventIndex = i
			}
		}
	}

	// The proxy should generate a synthetic ContinuedEvent for virtual Continue requests
	require.GreaterOrEqual(t, continuedEventIndex, 0,
		"Proxy should generate synthetic ContinuedEvent for virtual Continue request")
	t.Logf("Continued event at index %d, Stopped event at index %d", continuedEventIndex, stoppedEventIndex)
	assert.Less(t, continuedEventIndex, stoppedEventIndex,
		"Continued event should arrive before Stopped event")
	t.Log("✓ Event ordering verified: continued before stopped")

	// Extract the stopped event for further use
	stoppedEvent2, ok := events[len(events)-1].(*dap.StoppedEvent)
	require.True(t, ok, "Last event should be StoppedEvent")
	t.Logf("Stopped at second breakpoint: threadId=%d, reason=%s",
		stoppedEvent2.Body.ThreadId, stoppedEvent2.Body.Reason)

	// The fact that we received a second stopped event confirms:
	// 1. The virtual continue request worked
	// 2. The debuggee resumed execution
	// 3. The debuggee hit the next breakpoint and stopped again

	// Verify we're stopped at a breakpoint
	assert.Contains(t, stoppedEvent2.Body.Reason, "breakpoint", "Should be stopped at breakpoint")

	// Clear all breakpoints before continuing to avoid hitting the loop breakpoint multiple times
	t.Log("Clearing all breakpoints...")
	clearBpResp, clearBpErr := testClient.SetBreakpoints(ctx, debuggeeSource, []int{})
	require.NoError(t, clearBpErr, "Should clear breakpoints")
	assert.Empty(t, clearBpResp.Body.Breakpoints, "Should have no breakpoints")

	// Continue past the second breakpoint and wait for termination
	t.Log("Continuing to program termination...")
	contErr := testClient.Continue(ctx, stoppedEvent2.Body.ThreadId)
	require.NoError(t, contErr, "Continue should succeed")

	// Wait for terminated event
	t.Log("Waiting for terminated event...")
	termErr := testClient.WaitForTerminatedEvent(10 * time.Second)
	require.NoError(t, termErr, "Should receive terminated event")
	t.Log("Received terminated event")

	// Cleanup
	cancel()
	driverWg.Wait()

	t.Log("Virtual Continue request test completed successfully!")
}

// TestGRPC_E2E_VirtualSetBreakpoints tests that virtual setBreakpoints requests
// generate synthetic BreakpointEvents for added, removed, and changed breakpoints.
func TestGRPC_E2E_VirtualSetBreakpoints(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 60*time.Second)
	defer cancel()

	// Start Delve
	delve, startErr := startDelve(ctx, t)
	if startErr != nil {
		t.Fatalf("Failed to start Delve: %v", startErr)
	}
	defer delve.cleanup()

	// Setup gRPC server
	grpcListener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	if listenErr != nil {
		t.Fatalf("Failed to create gRPC listener: %v", listenErr)
	}

	testLog := testutil.NewLogForTesting("grpc-server")
	server := NewControlServer(ControlServerConfig{
		Listener:    grpcListener,
		BearerToken: "test-token",
		Logger:      testLog,
	})

	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		_ = server.Start(ctx)
	}()
	defer func() {
		server.Stop()
		serverWg.Wait()
	}()

	// Wait for gRPC server to be ready
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		conn, dialErr := net.DialTimeout("tcp", grpcListener.Addr().String(), 50*time.Millisecond)
		if dialErr != nil {
			return false, nil
		}
		conn.Close()
		return true, nil
	})
	require.NoError(t, waitErr, "gRPC server should be ready")

	// Setup resource key
	resourceKey := commonapi.NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: "test-ns",
			Name:      "virtual-breakpoints-test",
		},
		Kind: schema.GroupVersionKind{
			Group:   "dcp.io",
			Version: "v1",
			Kind:    "Executable",
		},
	}

	// Setup proxy infrastructure
	upstreamListener, upListenErr := net.Listen("tcp", "127.0.0.1:0")
	if upListenErr != nil {
		t.Fatalf("Failed to create upstream listener: %v", upListenErr)
	}
	defer upstreamListener.Close()

	downstreamConn, dialErr := net.Dial("tcp", delve.addr)
	if dialErr != nil {
		t.Fatalf("Failed to connect to Delve: %v", dialErr)
	}
	downstreamTransport := NewTCPTransport(downstreamConn)

	var upstreamConn net.Conn
	var acceptWg sync.WaitGroup
	acceptWg.Add(1)
	go func() {
		defer acceptWg.Done()
		upstreamConn, _ = upstreamListener.Accept()
	}()

	clientConn, clientDialErr := net.Dial("tcp", upstreamListener.Addr().String())
	if clientDialErr != nil {
		t.Fatalf("Failed to connect client: %v", clientDialErr)
	}
	clientTransport := NewTCPTransport(clientConn)
	testClient := NewTestClient(clientTransport)
	defer testClient.Close()

	acceptWg.Wait()
	upstreamTransport := NewTCPTransport(upstreamConn)

	proxy := NewProxy(upstreamTransport, downstreamTransport, ProxyConfig{
		Logger: testutil.NewLogForTesting("proxy"),
	})

	controlClient := NewControlClient(ControlClientConfig{
		Endpoint:    grpcListener.Addr().String(),
		BearerToken: "test-token",
		ResourceKey: resourceKey,
		Logger:      testutil.NewLogForTesting("client"),
	})

	driver := NewSessionDriver(proxy, controlClient, testutil.NewLogForTesting("driver"))

	var driverWg sync.WaitGroup
	driverWg.Add(1)
	go func() {
		defer driverWg.Done()
		_ = driver.Run(ctx)
	}()

	// Wait for session to be registered
	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		return server.GetSessionStatus(resourceKey) != nil, nil
	})
	require.NoError(t, waitErr, "Session should be registered")

	// Get debuggee paths
	debuggeeDir := getDebuggeeDir(t)
	debuggeeBinary := getDebuggeeBinary(t)
	debuggeeSource := filepath.Join(debuggeeDir, "debuggee.go")

	// === Initialize debug session ===
	t.Log("Initializing debug session...")
	_, initErr := testClient.Initialize(ctx)
	require.NoError(t, initErr, "Initialize should succeed")

	_, _ = testClient.WaitForEvent("initialized", 2*time.Second)

	// Launch
	t.Log("Launching debuggee...")
	launchErr := testClient.Launch(ctx, debuggeeBinary, false)
	require.NoError(t, launchErr, "Launch should succeed")

	// Configuration done (no breakpoints yet)
	t.Log("Sending configurationDone...")
	configErr := testClient.ConfigurationDone(ctx)
	require.NoError(t, configErr, "ConfigurationDone should succeed")

	// === Test 1: Add breakpoints via virtual request ===
	t.Log("Test 1: Adding breakpoints via virtual request...")
	setBpReq := &dap.SetBreakpointsRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "request",
			},
			Command: "setBreakpoints",
		},
		Arguments: dap.SetBreakpointsArguments{
			Source: dap.Source{
				Path: debuggeeSource,
			},
			Breakpoints: []dap.SourceBreakpoint{
				{Line: 18},
				{Line: 26},
			},
		},
	}
	setBpPayload, _ := json.Marshal(setBpReq)

	setBpRespPayload, virtualErr := server.SendVirtualRequest(ctx, resourceKey, setBpPayload, 5*time.Second)
	require.NoError(t, virtualErr, "Virtual setBreakpoints should succeed")

	var setBpResp dap.SetBreakpointsResponse
	parseErr := json.Unmarshal(setBpRespPayload, &setBpResp)
	require.NoError(t, parseErr, "Should parse setBreakpoints response")
	assert.True(t, setBpResp.Response.Success, "SetBreakpoints should succeed")
	require.Len(t, setBpResp.Body.Breakpoints, 2, "Should have 2 breakpoints")
	t.Logf("Added breakpoints: line %d (id=%d), line %d (id=%d)",
		setBpResp.Body.Breakpoints[0].Line, setBpResp.Body.Breakpoints[0].Id,
		setBpResp.Body.Breakpoints[1].Line, setBpResp.Body.Breakpoints[1].Id)

	// Collect any breakpoint events that were generated
	// The proxy should have generated "new" events for both breakpoints
	var bpEvents []*dap.BreakpointEvent
	for {
		event, eventErr := testClient.WaitForEvent("breakpoint", 500*time.Millisecond)
		if eventErr != nil {
			break // No more events
		}
		if bpEvent, ok := event.(*dap.BreakpointEvent); ok {
			bpEvents = append(bpEvents, bpEvent)
			t.Logf("Received BreakpointEvent: reason=%s, id=%d, line=%d",
				bpEvent.Body.Reason, bpEvent.Body.Breakpoint.Id, bpEvent.Body.Breakpoint.Line)
		}
	}

	// Verify we got "new" events for the added breakpoints
	require.Len(t, bpEvents, 2, "Should receive 2 breakpoint events for added breakpoints")
	for _, evt := range bpEvents {
		assert.Equal(t, "new", evt.Body.Reason, "Event reason should be 'new' for added breakpoints")
	}

	// === Test 2: Remove a breakpoint via virtual request ===
	t.Log("Test 2: Removing a breakpoint via virtual request...")
	removeBpReq := &dap.SetBreakpointsRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "request",
			},
			Command: "setBreakpoints",
		},
		Arguments: dap.SetBreakpointsArguments{
			Source: dap.Source{
				Path: debuggeeSource,
			},
			Breakpoints: []dap.SourceBreakpoint{
				{Line: 18}, // Keep only line 18, remove line 26
			},
		},
	}
	removeBpPayload, _ := json.Marshal(removeBpReq)

	removeBpRespPayload, removeErr := server.SendVirtualRequest(ctx, resourceKey, removeBpPayload, 5*time.Second)
	require.NoError(t, removeErr, "Virtual setBreakpoints (remove) should succeed")

	var removeBpResp dap.SetBreakpointsResponse
	parseRemoveErr := json.Unmarshal(removeBpRespPayload, &removeBpResp)
	require.NoError(t, parseRemoveErr, "Should parse setBreakpoints response")
	require.Len(t, removeBpResp.Body.Breakpoints, 1, "Should have 1 breakpoint")
	t.Logf("Remaining breakpoint: line %d", removeBpResp.Body.Breakpoints[0].Line)

	// Collect breakpoint events - should get a "removed" event
	bpEvents = nil
	for {
		event, eventErr := testClient.WaitForEvent("breakpoint", 500*time.Millisecond)
		if eventErr != nil {
			break
		}
		if bpEvent, ok := event.(*dap.BreakpointEvent); ok {
			bpEvents = append(bpEvents, bpEvent)
			t.Logf("Received BreakpointEvent: reason=%s, id=%d, line=%d",
				bpEvent.Body.Reason, bpEvent.Body.Breakpoint.Id, bpEvent.Body.Breakpoint.Line)
		}
	}

	// Verify we got a "removed" event
	require.Len(t, bpEvents, 1, "Should receive 1 breakpoint event for removed breakpoint")
	assert.Equal(t, "removed", bpEvents[0].Body.Reason, "Event reason should be 'removed'")
	assert.Equal(t, 26, bpEvents[0].Body.Breakpoint.Line, "Removed breakpoint should be on line 26")

	// === Test 3: Clear all breakpoints via virtual request ===
	t.Log("Test 3: Clearing all breakpoints via virtual request...")
	clearBpReq := &dap.SetBreakpointsRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "request",
			},
			Command: "setBreakpoints",
		},
		Arguments: dap.SetBreakpointsArguments{
			Source: dap.Source{
				Path: debuggeeSource,
			},
			Breakpoints: []dap.SourceBreakpoint{}, // Empty list
		},
	}
	clearBpPayload, _ := json.Marshal(clearBpReq)

	clearBpRespPayload, clearErr := server.SendVirtualRequest(ctx, resourceKey, clearBpPayload, 5*time.Second)
	require.NoError(t, clearErr, "Virtual setBreakpoints (clear) should succeed")

	var clearBpResp dap.SetBreakpointsResponse
	parseClearErr := json.Unmarshal(clearBpRespPayload, &clearBpResp)
	require.NoError(t, parseClearErr, "Should parse setBreakpoints response")
	assert.Empty(t, clearBpResp.Body.Breakpoints, "Should have no breakpoints")

	// Collect breakpoint events - should get a "removed" event for line 18
	bpEvents = nil
	for {
		event, eventErr := testClient.WaitForEvent("breakpoint", 500*time.Millisecond)
		if eventErr != nil {
			break
		}
		if bpEvent, ok := event.(*dap.BreakpointEvent); ok {
			bpEvents = append(bpEvents, bpEvent)
			t.Logf("Received BreakpointEvent: reason=%s, id=%d, line=%d",
				bpEvent.Body.Reason, bpEvent.Body.Breakpoint.Id, bpEvent.Body.Breakpoint.Line)
		}
	}

	require.Len(t, bpEvents, 1, "Should receive 1 breakpoint event for removed breakpoint")
	assert.Equal(t, "removed", bpEvents[0].Body.Reason, "Event reason should be 'removed'")
	assert.Equal(t, 18, bpEvents[0].Body.Breakpoint.Line, "Removed breakpoint should be on line 18")

	// Cleanup - disconnect
	t.Log("Disconnecting...")
	disconnCtx, disconnCancel := context.WithTimeout(ctx, 2*time.Second)
	_ = testClient.Disconnect(disconnCtx, true) // terminateDebuggee=true to end the session
	disconnCancel()

	// Cleanup
	cancel()
	driverWg.Wait()

	t.Log("Virtual setBreakpoints test completed successfully!")
}

// TestGRPC_E2E_SessionRejectionOnDuplicate tests that duplicate sessions are rejected.
func TestGRPC_E2E_SessionRejectionOnDuplicate(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 30*time.Second)
	defer cancel()

	// Start Delve
	delve, startErr := startDelve(ctx, t)
	if startErr != nil {
		t.Fatalf("Failed to start Delve: %v", startErr)
	}
	defer delve.cleanup()

	// Setup gRPC server
	grpcListener, _ := net.Listen("tcp", "127.0.0.1:0")
	testLog := testutil.NewLogForTesting("grpc-server")
	server := NewControlServer(ControlServerConfig{
		Listener:    grpcListener,
		BearerToken: "test-token",
		Logger:      testLog,
	})

	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		_ = server.Start(ctx)
	}()
	defer func() {
		server.Stop()
		serverWg.Wait()
	}()

	// Wait for gRPC server to be ready
	waitErr := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		conn, dialErr := net.DialTimeout("tcp", grpcListener.Addr().String(), 50*time.Millisecond)
		if dialErr != nil {
			return false, nil
		}
		conn.Close()
		return true, nil
	})
	require.NoError(t, waitErr, "gRPC server should be ready")

	// Same resource key for both sessions
	resourceKey := commonapi.NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: "test-ns",
			Name:      "duplicate-test",
		},
		Kind: schema.GroupVersionKind{
			Group:   "dcp.io",
			Version: "v1",
			Kind:    "Executable",
		},
	}

	// === First Session - should succeed ===
	upstreamListener1, _ := net.Listen("tcp", "127.0.0.1:0")
	defer upstreamListener1.Close()

	downstreamConn1, _ := net.Dial("tcp", delve.addr)
	downstreamTransport1 := NewTCPTransport(downstreamConn1)

	var upstreamConn1 net.Conn
	var acceptWg1 sync.WaitGroup
	acceptWg1.Add(1)
	go func() {
		defer acceptWg1.Done()
		upstreamConn1, _ = upstreamListener1.Accept()
	}()

	clientConn1, _ := net.Dial("tcp", upstreamListener1.Addr().String())
	clientTransport1 := NewTCPTransport(clientConn1)
	testClient1 := NewTestClient(clientTransport1)
	defer testClient1.Close()

	acceptWg1.Wait()
	upstreamTransport1 := NewTCPTransport(upstreamConn1)

	proxy1 := NewProxy(upstreamTransport1, downstreamTransport1, ProxyConfig{
		Logger: testutil.NewLogForTesting("proxy1"),
	})

	controlClient1 := NewControlClient(ControlClientConfig{
		Endpoint:    grpcListener.Addr().String(),
		BearerToken: "test-token",
		ResourceKey: resourceKey,
		Logger:      testutil.NewLogForTesting("client1"),
	})

	driver1 := NewSessionDriver(proxy1, controlClient1, testutil.NewLogForTesting("driver1"))

	var driver1Wg sync.WaitGroup
	driver1Wg.Add(1)
	go func() {
		defer driver1Wg.Done()
		_ = driver1.Run(ctx)
	}()

	// Wait for first session to be registered
	var sessionState *DebugSessionState
	waitErr = wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		sessionState = server.GetSessionStatus(resourceKey)
		return sessionState != nil, nil
	})
	require.NoError(t, waitErr, "First session should be registered")
	t.Logf("First session registered with status: %s", sessionState.Status.String())

	// === Second Session - should be rejected ===
	t.Log("Attempting second session with same resource key...")

	// Create second control client with same resource key
	controlClient2 := NewControlClient(ControlClientConfig{
		Endpoint:    grpcListener.Addr().String(),
		BearerToken: "test-token",
		ResourceKey: resourceKey,
		Logger:      testutil.NewLogForTesting("client2"),
	})

	// Try to connect - should fail with session rejected error
	connectErr := controlClient2.Connect(ctx)
	require.Error(t, connectErr, "Second session should be rejected")
	t.Logf("Second session rejected with error: %v", connectErr)

	assert.True(t,
		strings.Contains(connectErr.Error(), "AlreadyExists") ||
			strings.Contains(connectErr.Error(), "session already exists"),
		"Error should indicate duplicate session")

	// Cleanup
	cancel()
	driver1Wg.Wait()

	t.Log("Duplicate session rejection test completed!")
}
