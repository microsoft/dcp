/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/microsoft/dcp/pkg/commonapi"
)

// ErrSessionNotPreRegistered is returned when trying to claim a session that was not pre-registered.
var ErrSessionNotPreRegistered = errors.New("session not pre-registered")

// ErrSessionAlreadyClaimed is returned when trying to claim a session that is already connected.
var ErrSessionAlreadyClaimed = errors.New("session already claimed")

// ErrSessionParkingTimeout is returned when a parked connection times out waiting for registration.
var ErrSessionParkingTimeout = errors.New("session parking timeout")

// ErrConnectionAlreadyParked is returned when trying to park a connection for a resource that already has a parked connection.
var ErrConnectionAlreadyParked = errors.New("connection already parked for this resource")

// DefaultParkingTimeout is the default timeout for parked connections waiting for session registration.
const DefaultParkingTimeout = 30 * time.Second

// DefaultAdapterConnectionTimeout is the default timeout for connecting to the debug adapter.
const DefaultAdapterConnectionTimeout = 10 * time.Second

// DebugAdapterMode specifies how the debug adapter communicates.
type DebugAdapterMode int

const (
	// DebugAdapterModeStdio indicates the adapter uses stdin/stdout for DAP communication.
	DebugAdapterModeStdio DebugAdapterMode = iota

	// DebugAdapterModeTCPCallback indicates we start a listener and adapter connects to us.
	// Pass our address to the adapter via --client-addr or similar.
	DebugAdapterModeTCPCallback

	// DebugAdapterModeTCPConnect indicates we specify a port, adapter listens, we connect.
	// Use {{port}} placeholder in args which is replaced with allocated port.
	DebugAdapterModeTCPConnect
)

// String returns a string representation of the debug adapter mode.
func (m DebugAdapterMode) String() string {
	switch m {
	case DebugAdapterModeStdio:
		return "stdio"
	case DebugAdapterModeTCPCallback:
		return "tcp-callback"
	case DebugAdapterModeTCPConnect:
		return "tcp-connect"
	default:
		return "unknown"
	}
}

// ParseDebugAdapterMode parses a string into a DebugAdapterMode.
// Returns DebugAdapterModeStdio for empty string or unrecognized values.
func ParseDebugAdapterMode(s string) DebugAdapterMode {
	switch s {
	case "stdio", "":
		return DebugAdapterModeStdio
	case "tcp-callback":
		return DebugAdapterModeTCPCallback
	case "tcp-connect":
		return DebugAdapterModeTCPConnect
	default:
		return DebugAdapterModeStdio
	}
}

// EnvVar represents an environment variable with name and value.
type EnvVar struct {
	Name  string
	Value string
}

// DebugAdapterConfig holds the configuration for launching a debug adapter.
type DebugAdapterConfig struct {
	// Args contains the command and arguments to launch the debug adapter.
	// The first element is the executable path, subsequent elements are arguments.
	// May contain "{{port}}" placeholder for TCP modes.
	Args []string

	// Mode specifies how the adapter communicates (stdio, tcp-callback, or tcp-connect).
	// Default is DebugAdapterModeStdio.
	Mode DebugAdapterMode

	// Env contains environment variables to set for the adapter process.
	Env []EnvVar

	// ConnectionTimeout is the timeout for connecting to the adapter in TCP modes.
	// Default is DefaultAdapterConnectionTimeout.
	ConnectionTimeout time.Duration
}

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

	// parkingMu protects parked connection operations
	parkingMu         sync.Mutex
	parkedConnections map[string]*parkedConnection

	// rejectedSessions tracks sessions that have been rejected with the reason
	rejectedSessions map[string]string
}

// parkedConnection represents a connection waiting for session registration.
type parkedConnection struct {
	key       commonapi.NamespacedNameWithKind
	readyCh   chan *DebugAdapterConfig // Signals when session is registered
	rejectCh  chan string              // Signals when session is rejected (with reason)
	cancelCtx context.Context          // Context for cancellation
}

// sessionEntry holds session state and connection info.
type sessionEntry struct {
	state         DebugSessionState
	adapterConfig *DebugAdapterConfig    // Debug adapter launch configuration
	capabilities  map[string]interface{} // Debug adapter capabilities from InitializeResponse
	connected     bool                   // Whether a gRPC connection has claimed this session
	cancelFunc    func()                 // Called to terminate the session
}

// NewSessionMap creates a new session map.
func NewSessionMap() *SessionMap {
	return &SessionMap{
		sessions:          make(map[string]*sessionEntry),
		events:            make(chan SessionEvent, 100),
		parkedConnections: make(map[string]*parkedConnection),
		rejectedSessions:  make(map[string]string),
	}
}

// resourceKey returns the map key for a NamespacedNameWithKind.
func resourceKey(nnk commonapi.NamespacedNameWithKind) string {
	return nnk.String()
}

// PreRegisterSession pre-registers a debug session for the given resource with adapter configuration.
// This is called by controllers when an Executable with DebugAdapterLaunch is created or becomes debuggable.
// If a connection is parked waiting for this resource, it will be woken up.
// Returns ErrSessionRejected if a session already exists for the resource.
func (m *SessionMap) PreRegisterSession(
	key commonapi.NamespacedNameWithKind,
	config *DebugAdapterConfig,
) error {
	m.mu.Lock()

	k := resourceKey(key)
	if _, exists := m.sessions[k]; exists {
		m.mu.Unlock()
		return ErrSessionRejected
	}

	// Clear any previous rejection for this resource
	m.parkingMu.Lock()
	delete(m.rejectedSessions, k)
	parked := m.parkedConnections[k]
	if parked != nil {
		delete(m.parkedConnections, k)
	}
	m.parkingMu.Unlock()

	m.sessions[k] = &sessionEntry{
		state: DebugSessionState{
			ResourceKey: key,
			Status:      DebugSessionStatusConnecting,
			LastUpdated: time.Now(),
		},
		adapterConfig: config,
		connected:     false,
	}

	m.mu.Unlock()

	// Wake up parked connection if one exists
	if parked != nil {
		select {
		case parked.readyCh <- config:
		default:
		}
	}

	return nil
}

// ClaimSession claims a pre-registered session when a gRPC connection is established.
// Returns ErrSessionNotPreRegistered if the session was not pre-registered.
// Returns ErrSessionAlreadyClaimed if another connection already claimed this session.
// The cancelFunc is called when TerminateSession is invoked.
func (m *SessionMap) ClaimSession(
	key commonapi.NamespacedNameWithKind,
	cancelFunc func(),
) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := resourceKey(key)
	entry, exists := m.sessions[k]
	if !exists {
		return ErrSessionNotPreRegistered
	}

	if entry.connected {
		return ErrSessionAlreadyClaimed
	}

	entry.connected = true
	entry.cancelFunc = cancelFunc
	entry.state.LastUpdated = time.Now()

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

// ReleaseSession releases a claimed session without removing it from the map.
// This allows the session to be claimed again by a new gRPC connection.
func (m *SessionMap) ReleaseSession(key commonapi.NamespacedNameWithKind) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := resourceKey(key)
	entry, exists := m.sessions[k]
	if !exists {
		return
	}

	if entry.connected {
		entry.connected = false
		entry.cancelFunc = nil
		entry.capabilities = nil
		entry.state.Status = DebugSessionStatusConnecting
		entry.state.LastUpdated = time.Now()
		entry.state.ErrorMessage = ""

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

// GetAdapterConfig returns the debug adapter configuration for a pre-registered session.
// Returns nil if the session is not found.
func (m *SessionMap) GetAdapterConfig(key commonapi.NamespacedNameWithKind) *DebugAdapterConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	k := resourceKey(key)
	entry, exists := m.sessions[k]
	if !exists {
		return nil
	}

	return entry.adapterConfig
}

// ParkConnection parks a connection to wait for session registration.
// The connection will wait until the session is registered, rejected, context is cancelled, or timeout.
// Returns the adapter config if the session is registered, or an error if rejected/timed out.
// Only one connection can be parked per resource key.
func (m *SessionMap) ParkConnection(
	ctx context.Context,
	key commonapi.NamespacedNameWithKind,
	timeout time.Duration,
) (*DebugAdapterConfig, error) {
	k := resourceKey(key)

	// Check if session is already registered
	m.mu.RLock()
	entry, exists := m.sessions[k]
	m.mu.RUnlock()
	if exists {
		return entry.adapterConfig, nil
	}

	// Check for rejection
	m.parkingMu.Lock()
	if reason, rejected := m.rejectedSessions[k]; rejected {
		m.parkingMu.Unlock()
		return nil, errors.New(reason)
	}

	// Check if already parked
	if _, alreadyParked := m.parkedConnections[k]; alreadyParked {
		m.parkingMu.Unlock()
		return nil, ErrConnectionAlreadyParked
	}

	// Create parked connection
	parked := &parkedConnection{
		key:       key,
		readyCh:   make(chan *DebugAdapterConfig, 1),
		rejectCh:  make(chan string, 1),
		cancelCtx: ctx,
	}
	m.parkedConnections[k] = parked
	m.parkingMu.Unlock()

	// Clean up on exit
	defer func() {
		m.parkingMu.Lock()
		if m.parkedConnections[k] == parked {
			delete(m.parkedConnections, k)
		}
		m.parkingMu.Unlock()
	}()

	// Wait for registration, rejection, context cancellation, or timeout
	if timeout <= 0 {
		timeout = DefaultParkingTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case config := <-parked.readyCh:
		return config, nil
	case reason := <-parked.rejectCh:
		return nil, errors.New(reason)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrSessionParkingTimeout
	}
}

// RejectSession marks a session as rejected with the given reason.
// Any parked connection for this resource will be woken up with the rejection reason.
// This is called when an executable fails to start or terminates before debug session can be established.
func (m *SessionMap) RejectSession(key commonapi.NamespacedNameWithKind, reason string) {
	k := resourceKey(key)

	m.parkingMu.Lock()
	m.rejectedSessions[k] = reason
	parked := m.parkedConnections[k]
	if parked != nil {
		delete(m.parkedConnections, k)
	}
	m.parkingMu.Unlock()

	// Wake up parked connection with rejection
	if parked != nil {
		select {
		case parked.rejectCh <- reason:
		default:
		}
	}
}

// IsSessionRejected checks if a session has been rejected.
// Returns the rejection reason and true if rejected, empty string and false otherwise.
func (m *SessionMap) IsSessionRejected(key commonapi.NamespacedNameWithKind) (string, bool) {
	k := resourceKey(key)
	m.parkingMu.Lock()
	reason, rejected := m.rejectedSessions[k]
	m.parkingMu.Unlock()
	return reason, rejected
}

// ClearRejection clears any rejection for the given resource.
// This is called when a resource is deleted or re-created.
func (m *SessionMap) ClearRejection(key commonapi.NamespacedNameWithKind) {
	k := resourceKey(key)
	m.parkingMu.Lock()
	delete(m.rejectedSessions, k)
	m.parkingMu.Unlock()
}

// SetCapabilities stores the debug adapter capabilities for a session.
func (m *SessionMap) SetCapabilities(key commonapi.NamespacedNameWithKind, capabilities map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := resourceKey(key)
	entry, exists := m.sessions[k]
	if !exists {
		return
	}

	entry.capabilities = capabilities
}

// GetCapabilities returns the debug adapter capabilities for a session.
// Returns nil if the session is not found or capabilities have not been set.
func (m *SessionMap) GetCapabilities(key commonapi.NamespacedNameWithKind) map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	k := resourceKey(key)
	entry, exists := m.sessions[k]
	if !exists {
		return nil
	}

	return entry.capabilities
}

// IsSessionConnected returns whether a gRPC connection has claimed the session.
func (m *SessionMap) IsSessionConnected(key commonapi.NamespacedNameWithKind) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	k := resourceKey(key)
	entry, exists := m.sessions[k]
	if !exists {
		return false
	}

	return entry.connected
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
