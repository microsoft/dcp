/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortTempDir creates a short temporary directory for socket tests.
// macOS has a ~104 character limit for Unix socket paths.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, dirErr := os.MkdirTemp("", "sck")
	require.NoError(t, dirErr)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestDapBridge_Creation(t *testing.T) {
	t.Parallel()

	config := BridgeConfig{
		SessionID: "test-session",
	}

	bridge := NewDapBridge(config)

	assert.NotNil(t, bridge)
}

func TestDapBridge_RunWithConnection(t *testing.T) {
	t.Parallel()

	// Test that RunWithConnection starts and can be cancelled
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	config := BridgeConfig{
		SessionID: "session-456",
		AdapterConfig: &DebugAdapterConfig{
			Args: []string{"echo", "test"}, // Simple command
			Mode: DebugAdapterModeStdio,
		},
	}

	bridge := NewDapBridge(config)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Run bridge with pre-connected connection
	// It will fail to properly run the adapter but will start correctly
	errCh := make(chan error, 1)
	go func() {
		errCh <- bridge.RunWithConnection(ctx, serverConn)
	}()

	// Cancel to shutdown
	cancel()

	// Wait for bridge to finish
	select {
	case <-errCh:
		// Good
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not shut down in time")
	}
}

func TestDapBridge_RunInTerminalUsed(t *testing.T) {
	t.Parallel()

	config := BridgeConfig{
		SessionID: "session",
	}

	bridge := NewDapBridge(config)

	// Initially false
	assert.False(t, bridge.runInTerminalUsed.Load())
}

func TestDapBridge_Done(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	config := BridgeConfig{
		SessionID: "session",
		AdapterConfig: &DebugAdapterConfig{
			Args: []string{"echo"},
			Mode: DebugAdapterModeStdio,
		},
	}

	bridge := NewDapBridge(config)

	// Done channel should not be closed initially
	select {
	case <-bridge.terminateCh:
		t.Fatal("Done channel should not be closed before running")
	default:
		// Expected
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start bridge
	go func() {
		_ = bridge.RunWithConnection(ctx, serverConn)
	}()

	// Give it time to start
	time.Sleep(50 * time.Millisecond)

	// Cancel to cause termination
	cancel()

	// Done channel should be closed after termination
	select {
	case <-bridge.terminateCh:
		// Expected
	case <-time.After(2 * time.Second):
		t.Fatal("Done channel should be closed after termination")
	}
}

func TestBridgeManager_SocketPath(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{})

	// Before Start(), SocketPath() returns empty string since no listener exists yet
	assert.Empty(t, manager.SocketPath())
}

func TestBridgeManager_DefaultSocketNamePrefix(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{})

	// Should use default prefix
	assert.Equal(t, DefaultSocketNamePrefix, manager.socketPrefix)
}

func TestBridgeManager_StartAndReady(t *testing.T) {
	t.Parallel()

	socketDir := shortTempDir(t)

	manager := NewBridgeManager(BridgeManagerConfig{
		SocketDir: socketDir,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start in background
	go func() {
		_ = manager.Start(ctx)
	}()

	// Wait for ready
	select {
	case <-manager.Ready():
		// Expected — SocketPath should now be set
		assert.NotEmpty(t, manager.SocketPath())
		assert.Contains(t, manager.SocketPath(), DefaultSocketNamePrefix)
	case <-time.After(1 * time.Second):
		t.Fatal("manager did not become ready in time")
	}

	cancel()
}

func TestBridgeManager_DuplicateSession(t *testing.T) {
	t.Parallel()

	// Test that a second connection for the same session is rejected

	socketDir := shortTempDir(t)
	manager := NewBridgeManager(BridgeManagerConfig{
		SocketDir:        socketDir,
		HandshakeTimeout: 2 * time.Second,
	})
	_, _ = manager.RegisterSession("dup-session", "token")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = manager.Start(ctx)
	}()

	<-manager.Ready()

	socketPath := manager.SocketPath()

	// First connection - will fail because no debug adapter config in handshake,
	// but it should mark the session as connected first
	conn1, err1 := net.Dial("unix", socketPath)
	require.NoError(t, err1)
	defer conn1.Close()

	// Send a handshake without debug adapter config - it will fail but mark connected
	writer := NewHandshakeWriter(conn1)
	_ = writer.WriteRequest(&HandshakeRequest{
		Token:     "token",
		SessionID: "dup-session",
		// No DebugAdapterConfig - this will cause failure but connected flag is set first
	})

	// Give time for first connection to be processed
	time.Sleep(200 * time.Millisecond)

	// Second connection for the same session
	conn2, err2 := net.Dial("unix", socketPath)
	require.NoError(t, err2)
	defer conn2.Close()

	// This handshake should fail because session is already connected
	handshakeErr := performClientHandshake(conn2, "token", "dup-session", "")
	assert.Error(t, handshakeErr, "second connection should be rejected")

	cancel()
}
