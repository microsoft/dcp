//go:build integration

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/dcp/pkg/testutil"
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
