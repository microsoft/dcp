/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBridgeManager_RegisterSession(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	session, err := manager.RegisterSession("test-session-1", "test-token-123")
	require.NoError(t, err)
	require.NotNil(t, session)

	assert.Equal(t, "test-session-1", session.ID)
	assert.Equal(t, "test-token-123", session.Token)
	assert.Equal(t, BridgeSessionStateCreated, session.State)
	assert.False(t, session.Connected)
}

func TestBridgeManager_RegisterSession_DuplicateID(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, sessionErr := manager.RegisterSession("dup-session", "token1")
	require.NoError(t, sessionErr)

	_, dupErr := manager.RegisterSession("dup-session", "token2")
	assert.ErrorIs(t, dupErr, ErrBridgeSessionAlreadyExists)
}

func TestBridgeManager_ValidateHandshake_InvalidToken(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, regErr := manager.RegisterSession("token-session", "correct-token")
	require.NoError(t, regErr)

	_, validateErr := manager.validateHandshake("token-session", "wrong-token")
	assert.ErrorIs(t, validateErr, ErrBridgeSessionInvalidToken)
}

func TestBridgeManager_ValidateHandshake_SessionNotFound(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, validateErr := manager.validateHandshake("nonexistent", "any-token")
	assert.ErrorIs(t, validateErr, ErrBridgeSessionNotFound)
}

func TestBridgeManager_MarkSessionConnected(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	session, regErr := manager.RegisterSession("connect-session", "test-token")
	require.NoError(t, regErr)
	assert.False(t, session.Connected)

	// First connection should succeed
	connectErr := manager.markSessionConnected("connect-session")
	require.NoError(t, connectErr)
	assert.True(t, session.Connected)

	// Second connection attempt should fail
	connectErr2 := manager.markSessionConnected("connect-session")
	assert.ErrorIs(t, connectErr2, ErrBridgeSessionAlreadyConnected)
}

func TestBridgeManager_MarkSessionConnected_NotFound(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	connectErr := manager.markSessionConnected("nonexistent")
	assert.ErrorIs(t, connectErr, ErrBridgeSessionNotFound)
}

func TestBridgeManager_MarkSessionDisconnected(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, regErr := manager.RegisterSession("disconnect-session", "test-token")
	require.NoError(t, regErr)

	// Mark connected, then disconnect
	connectErr := manager.markSessionConnected("disconnect-session")
	require.NoError(t, connectErr)

	manager.markSessionDisconnected("disconnect-session")

	// Should be able to connect again after disconnect
	reconnectErr := manager.markSessionConnected("disconnect-session")
	assert.NoError(t, reconnectErr)
}

func TestBridgeManager_MarkSessionDisconnected_NotFound(t *testing.T) {
	t.Parallel()

	// Should be a no-op, not panic
	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())
	manager.markSessionDisconnected("nonexistent")
}

func TestBridgeManager_RegisterChildSession(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, regErr := manager.RegisterSession("parent-session", "parent-token")
	require.NoError(t, regErr)

	child, childErr := manager.RegisterChildSession("parent-session", "child-session-1")
	require.NoError(t, childErr)
	require.NotNil(t, child)

	assert.Equal(t, "child-session-1", child.ID)
	assert.Equal(t, "parent-token", child.Token, "child should inherit parent token")
	assert.Equal(t, "parent-session", child.ParentSessionID)
	assert.Equal(t, BridgeSessionStateCreated, child.State)
	assert.False(t, child.Connected)
}

func TestBridgeManager_RegisterChildSession_ParentNotFound(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, childErr := manager.RegisterChildSession("nonexistent", "child-1")
	assert.ErrorIs(t, childErr, ErrBridgeSessionNotFound)
}

func TestBridgeManager_RegisterChildSession_DuplicateID(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, regErr := manager.RegisterSession("parent", "token")
	require.NoError(t, regErr)

	_, child1Err := manager.RegisterChildSession("parent", "child-dup")
	require.NoError(t, child1Err)

	_, child2Err := manager.RegisterChildSession("parent", "child-dup")
	assert.ErrorIs(t, child2Err, ErrBridgeSessionAlreadyExists)
}

func TestBridgeManager_RegisterChildSession_InheritsAdapterAddress(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	parent, regErr := manager.RegisterSession("parent-addr", "token")
	require.NoError(t, regErr)
	parent.AdapterAddress = "127.0.0.1:12345"

	child, childErr := manager.RegisterChildSession("parent-addr", "child-addr")
	require.NoError(t, childErr)

	assert.Equal(t, "127.0.0.1:12345", child.AdapterAddress,
		"child should inherit parent adapter address")
}

func TestBridgeManager_RegisterChildSession_InheritsAdapterConfig(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	parent, regErr := manager.RegisterSession("parent-cfg", "token")
	require.NoError(t, regErr)
	parent.AdapterConfig = &DebugAdapterConfig{
		Args: []string{"node", "debugger.js"},
		Mode: DebugAdapterModeStdio,
	}

	child, childErr := manager.RegisterChildSession("parent-cfg", "child-cfg")
	require.NoError(t, childErr)

	require.NotNil(t, child.AdapterConfig, "child should inherit parent adapter config")
	assert.Equal(t, "node", child.AdapterConfig.Args[0])
	assert.Equal(t, DebugAdapterModeStdio, child.AdapterConfig.Mode)
}

func TestBridgeManager_ChildSessionHandshakeValidation(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, regErr := manager.RegisterSession("parent-hs", "parent-token")
	require.NoError(t, regErr)

	_, childErr := manager.RegisterChildSession("parent-hs", "child-hs")
	require.NoError(t, childErr)

	// Child session should validate with parent's token
	session, validateErr := manager.validateHandshake("child-hs", "parent-token")
	require.NoError(t, validateErr)
	assert.Equal(t, "child-hs", session.ID)

	// Wrong token should fail
	_, wrongErr := manager.validateHandshake("child-hs", "wrong-token")
	assert.ErrorIs(t, wrongErr, ErrBridgeSessionInvalidToken)
}

func TestBridgeManager_CancelChildSessions(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, regErr := manager.RegisterSession("parent-cancel", "token")
	require.NoError(t, regErr)

	child1, _ := manager.RegisterChildSession("parent-cancel", "child-cancel-1")
	child2, _ := manager.RegisterChildSession("parent-cancel", "child-cancel-2")

	// Assign cancel functions to children (simulating what runBridge does)
	child1Done := make(chan struct{})
	child2Done := make(chan struct{})
	_, child1Cancel := context.WithCancel(context.Background())
	_, child2Cancel := context.WithCancel(context.Background())

	child1.cancelFunc = func() {
		child1Cancel()
		close(child1Done)
	}
	child2.cancelFunc = func() {
		child2Cancel()
		close(child2Done)
	}

	// Cancel all children of parent
	manager.cancelChildSessions("parent-cancel")

	// Both children should be cancelled
	select {
	case <-child1Done:
	case <-time.After(time.Second):
		t.Fatal("child1 was not cancelled")
	}
	select {
	case <-child2Done:
	case <-time.After(time.Second):
		t.Fatal("child2 was not cancelled")
	}
}

func TestBridgeManager_HandleStartDebugging(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	parent, regErr := manager.RegisterSession("parent-sd", "token")
	require.NoError(t, regErr)
	parent.AdapterAddress = "127.0.0.1:9999"

	childID, handleErr := manager.handleStartDebugging("parent-sd", map[string]any{"type": "node"}, "launch")
	require.NoError(t, handleErr)
	assert.Equal(t, "parent-sd:0", childID)

	// Verify child was registered
	manager.mu.Lock()
	child, exists := manager.sessions[childID]
	manager.mu.Unlock()
	require.True(t, exists, "child session should be registered")
	assert.Equal(t, "parent-sd", child.ParentSessionID)
	assert.Equal(t, "127.0.0.1:9999", child.AdapterAddress)
	assert.Equal(t, "token", child.Token)

	// Second child should get incremented ID
	childID2, handleErr2 := manager.handleStartDebugging("parent-sd", nil, "attach")
	require.NoError(t, handleErr2)
	assert.Equal(t, "parent-sd:1", childID2)
}

func TestBridgeManager_HandleStartDebugging_ParentNotFound(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{}, logr.Discard())

	_, handleErr := manager.handleStartDebugging("nonexistent", nil, "launch")
	assert.ErrorIs(t, handleErr, ErrBridgeSessionNotFound)
}
