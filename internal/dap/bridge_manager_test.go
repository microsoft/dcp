/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBridgeManager_RegisterSession(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{Logger: logr.Discard()})

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

	manager := NewBridgeManager(BridgeManagerConfig{Logger: logr.Discard()})

	_, sessionErr := manager.RegisterSession("dup-session", "token1")
	require.NoError(t, sessionErr)

	_, dupErr := manager.RegisterSession("dup-session", "token2")
	assert.ErrorIs(t, dupErr, ErrBridgeSessionAlreadyExists)
}

func TestBridgeManager_ValidateHandshake_InvalidToken(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{Logger: logr.Discard()})

	_, regErr := manager.RegisterSession("token-session", "correct-token")
	require.NoError(t, regErr)

	_, validateErr := manager.validateHandshake("token-session", "wrong-token")
	assert.ErrorIs(t, validateErr, ErrBridgeSessionInvalidToken)
}

func TestBridgeManager_ValidateHandshake_SessionNotFound(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{Logger: logr.Discard()})

	_, validateErr := manager.validateHandshake("nonexistent", "any-token")
	assert.ErrorIs(t, validateErr, ErrBridgeSessionNotFound)
}

func TestBridgeManager_MarkSessionConnected(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{Logger: logr.Discard()})

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

	manager := NewBridgeManager(BridgeManagerConfig{Logger: logr.Discard()})

	connectErr := manager.markSessionConnected("nonexistent")
	assert.ErrorIs(t, connectErr, ErrBridgeSessionNotFound)
}

func TestBridgeManager_MarkSessionDisconnected(t *testing.T) {
	t.Parallel()

	manager := NewBridgeManager(BridgeManagerConfig{Logger: logr.Discard()})

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
	manager := NewBridgeManager(BridgeManagerConfig{Logger: logr.Discard()})
	manager.markSessionDisconnected("nonexistent")
}
