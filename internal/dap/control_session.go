/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"sync"
	"time"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// DebugSessionStatus represents the current state of a debug session.
type DebugSessionStatus int

const (
	// DebugSessionStatusConnecting indicates the session is being established.
	DebugSessionStatusConnecting DebugSessionStatus = iota

	// DebugSessionStatusInitializing indicates the debug adapter is initializing.
	DebugSessionStatusInitializing

	// DebugSessionStatusAttached indicates the debugger is attached and running.
	DebugSessionStatusAttached

	// DebugSessionStatusStopped indicates the debugger is stopped at a breakpoint.
	DebugSessionStatusStopped

	// DebugSessionStatusTerminated indicates the debug session has ended.
	DebugSessionStatusTerminated

	// DebugSessionStatusError indicates the debug session encountered an error.
	DebugSessionStatusError
)

// String returns a string representation of the debug session status.
func (s DebugSessionStatus) String() string {
	switch s {
	case DebugSessionStatusConnecting:
		return "connecting"
	case DebugSessionStatusInitializing:
		return "initializing"
	case DebugSessionStatusAttached:
		return "attached"
	case DebugSessionStatusStopped:
		return "stopped"
	case DebugSessionStatusTerminated:
		return "terminated"
	case DebugSessionStatusError:
		return "error"
	default:
		return "unknown"
	}
}

// DebugSessionState holds the current state of a debug session.
type DebugSessionState struct {
	// ResourceKey identifies the resource being debugged.
	ResourceKey commonapi.NamespacedNameWithKind

	// Status is the current session status.
	Status DebugSessionStatus

	// LastUpdated is when the status was last updated.
	LastUpdated time.Time

	// ErrorMessage contains error details when Status is DebugSessionStatusError.
	ErrorMessage string
}

// SessionEventType identifies the type of session lifecycle event.
type SessionEventType int

const (
	// SessionEventConnected indicates a new session was established.
	SessionEventConnected SessionEventType = iota

	// SessionEventDisconnected indicates a session was disconnected.
	SessionEventDisconnected

	// SessionEventStatusChanged indicates the session status changed.
	SessionEventStatusChanged

	// SessionEventTerminatedByServer indicates the server terminated the session.
	SessionEventTerminatedByServer
)

// SessionEvent represents a session lifecycle event.
type SessionEvent struct {
	// ResourceKey identifies the resource.
	ResourceKey commonapi.NamespacedNameWithKind

	// EventType is the type of event.
	EventType SessionEventType

	// Status is the current status (for StatusChanged events).
	Status DebugSessionStatus
}

// SessionMap manages active debug sessions with single-session-per-resource enforcement.
type SessionMap struct {
	mu       sync.RWMutex
	sessions map[string]*sessionEntry
	events   chan SessionEvent
}

// sessionEntry holds session state and connection info.
type sessionEntry struct {
	state      DebugSessionState
	cancelFunc func() // Called to terminate the session
}

// NewSessionMap creates a new session map.
func NewSessionMap() *SessionMap {
	return &SessionMap{
		sessions: make(map[string]*sessionEntry),
		events:   make(chan SessionEvent, 100),
	}
}

// resourceKey returns the map key for a NamespacedNameWithKind.
func resourceKey(nnk commonapi.NamespacedNameWithKind) string {
	return nnk.String()
}

// RegisterSession registers a new debug session for the given resource.
// Returns ErrSessionRejected if a session already exists for the resource.
// The cancelFunc is called when TerminateSession is invoked.
func (m *SessionMap) RegisterSession(
	key commonapi.NamespacedNameWithKind,
	cancelFunc func(),
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := resourceKey(key)
	if _, exists := m.sessions[k]; exists {
		return ErrSessionRejected
	}

	m.sessions[k] = &sessionEntry{
		state: DebugSessionState{
			ResourceKey: key,
			Status:      DebugSessionStatusConnecting,
			LastUpdated: time.Now(),
		},
		cancelFunc: cancelFunc,
	}

	// Send connected event
	select {
	case m.events <- SessionEvent{
		ResourceKey: key,
		EventType:   SessionEventConnected,
		Status:      DebugSessionStatusConnecting,
	}:
	default:
		// Event channel full, drop event
	}

	return nil
}

// DeregisterSession removes a session from the map.
func (m *SessionMap) DeregisterSession(key commonapi.NamespacedNameWithKind) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := resourceKey(key)
	if _, exists := m.sessions[k]; exists {
		delete(m.sessions, k)

		// Send disconnected event
		select {
		case m.events <- SessionEvent{
			ResourceKey: key,
			EventType:   SessionEventDisconnected,
		}:
		default:
			// Event channel full, drop event
		}
	}
}

// GetSessionStatus returns the current state of a session, or nil if not found.
func (m *SessionMap) GetSessionStatus(key commonapi.NamespacedNameWithKind) *DebugSessionState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	k := resourceKey(key)
	entry, exists := m.sessions[k]
	if !exists {
		return nil
	}

	// Return a copy to avoid races
	stateCopy := entry.state
	return &stateCopy
}

// UpdateSessionStatus updates the status of an existing session.
func (m *SessionMap) UpdateSessionStatus(
	key commonapi.NamespacedNameWithKind,
	status DebugSessionStatus,
	errorMsg string,
) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := resourceKey(key)
	entry, exists := m.sessions[k]
	if !exists {
		return
	}

	entry.state.Status = status
	entry.state.LastUpdated = time.Now()
	entry.state.ErrorMessage = errorMsg

	// Send status changed event
	select {
	case m.events <- SessionEvent{
		ResourceKey: key,
		EventType:   SessionEventStatusChanged,
		Status:      status,
	}:
	default:
		// Event channel full, drop event
	}
}

// TerminateSession terminates a session by calling its cancel function.
// The session is not removed from the map; the session should deregister itself.
func (m *SessionMap) TerminateSession(key commonapi.NamespacedNameWithKind) {
	m.mu.RLock()
	k := resourceKey(key)
	entry, exists := m.sessions[k]
	m.mu.RUnlock()

	if !exists {
		return
	}

	// Send terminated by server event
	select {
	case m.events <- SessionEvent{
		ResourceKey: key,
		EventType:   SessionEventTerminatedByServer,
	}:
	default:
		// Event channel full, drop event
	}

	// Call cancel function outside the lock to avoid deadlocks
	if entry.cancelFunc != nil {
		entry.cancelFunc()
	}
}

// SessionEvents returns a channel that receives session lifecycle events.
// The channel has a buffer and events may be dropped if the consumer is slow.
func (m *SessionMap) SessionEvents() <-chan SessionEvent {
	return m.events
}

// ActiveSessions returns a list of all active session resource keys.
func (m *SessionMap) ActiveSessions() []commonapi.NamespacedNameWithKind {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]commonapi.NamespacedNameWithKind, 0, len(m.sessions))
	for _, entry := range m.sessions {
		keys = append(keys, entry.state.ResourceKey)
	}
	return keys
}
