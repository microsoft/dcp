/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/internal/networking"
	"github.com/microsoft/dcp/pkg/process"
)

const (
	// DefaultSocketNamePrefix is the default prefix for the DAP bridge socket name.
	// A random suffix is appended to support multiple DCP instances.
	DefaultSocketNamePrefix = "dcp-dap-"

	// DefaultHandshakeTimeout is the default timeout for reading the handshake.
	DefaultHandshakeTimeout = 30 * time.Second
)

// BridgeSessionState represents the current state of a bridge session.
type BridgeSessionState int

const (
	// BridgeSessionStateCreated indicates the session has been registered but bridge not started.
	BridgeSessionStateCreated BridgeSessionState = iota

	// BridgeSessionStateConnected indicates the IDE is connected and debugging is active.
	BridgeSessionStateConnected

	// BridgeSessionStateTerminated indicates the session has ended.
	BridgeSessionStateTerminated

	// BridgeSessionStateError indicates the session encountered an error.
	BridgeSessionStateError
)

// String returns a string representation of the session state.
func (s BridgeSessionState) String() string {
	switch s {
	case BridgeSessionStateCreated:
		return "created"
	case BridgeSessionStateConnected:
		return "connected"
	case BridgeSessionStateTerminated:
		return "terminated"
	case BridgeSessionStateError:
		return "error"
	default:
		return "unknown"
	}
}

// BridgeSession holds the state for a debug bridge session.
type BridgeSession struct {
	// ID is the unique identifier for this session.
	ID string

	// Token is the authentication token for this session.
	// This is the same token used for IDE authentication (reused, not generated).
	Token string

	// State is the current session state.
	State BridgeSessionState

	// Connected indicates whether an IDE has connected to this session.
	// Only one connection is allowed per session.
	Connected bool

	// CreatedAt is when the session was created.
	CreatedAt time.Time

	// Error holds any error message if State is BridgeSessionStateError.
	Error string
}

// Error constants for session management.
var (
	ErrBridgeSessionNotFound         = errors.New("bridge session not found")
	ErrBridgeSessionAlreadyExists    = errors.New("bridge session already exists")
	ErrBridgeSessionInvalidToken     = errors.New("invalid session token")
	ErrBridgeSessionAlreadyConnected = errors.New("session already connected")
)

// BridgeConnectionHandler is called when a new bridge connection is established,
// after the handshake has been validated. It returns the OutputHandler and stdout/stderr
// writers to use for the bridge session. This allows the caller to wire debug adapter
// output into the appropriate log files for the executable resource.
//
// sessionID is the bridge session identifier (typically the Executable UID).
// runID is the IDE run session identifier provided during the handshake.
//
// If the handler returns a nil OutputHandler, output events from the debug adapter will
// not be captured (they are still forwarded to the IDE). If stdout/stderr writers are nil,
// runInTerminal process output will be discarded.
type BridgeConnectionHandler func(sessionID string, runID string) (OutputHandler, io.Writer, io.Writer)

// BridgeManagerConfig contains configuration for the BridgeManager.
type BridgeManagerConfig struct {
	// SocketDir is the root directory where the secure socket directory will be created.
	// If empty, os.UserCacheDir() is used.
	SocketDir string

	// SocketNamePrefix is the prefix for the socket file name.
	// A random suffix is appended to support multiple DCP instances.
	// If empty, DefaultSocketNamePrefix is used.
	SocketNamePrefix string

	// Executor is the process executor for debug adapter processes.
	// If nil, a new executor will be created.
	Executor process.Executor

	// Logger for bridge manager operations.
	Logger logr.Logger

	// HandshakeTimeout is the timeout for reading the handshake from a connection.
	// If zero, defaults to DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration

	// ConnectionHandler is called when a bridge connection is established to resolve
	// the OutputHandler and stdout/stderr writers for the session. If nil, output
	// from debug sessions will not be captured to executable log files.
	ConnectionHandler BridgeConnectionHandler
}

// BridgeManager manages DAP bridge sessions and a shared Unix socket for IDE connections.
// It accepts incoming connections, performs handshake validation, and dispatches
// connections to the appropriate bridge sessions.
type BridgeManager struct {
	config   BridgeManagerConfig
	listener *networking.SecureSocketListener
	log      logr.Logger
	executor process.Executor

	// Socket configuration
	socketDir    string
	socketPrefix string
	readyCh      chan struct{}
	readyOnce    sync.Once

	// mu protects sessions and activeBridges.
	mu            sync.Mutex
	sessions      map[string]*BridgeSession
	activeBridges map[string]*DapBridge
}

// NewBridgeManager creates a new BridgeManager with the given configuration.
func NewBridgeManager(config BridgeManagerConfig) *BridgeManager {
	log := config.Logger
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	executor := config.Executor
	if executor == nil {
		executor = process.NewOSExecutor(log)
	}

	socketDir := config.SocketDir
	socketPrefix := config.SocketNamePrefix
	if socketPrefix == "" {
		socketPrefix = DefaultSocketNamePrefix
	}

	return &BridgeManager{
		config:        config,
		log:           log,
		executor:      executor,
		socketDir:     socketDir,
		socketPrefix:  socketPrefix,
		readyCh:       make(chan struct{}),
		sessions:      make(map[string]*BridgeSession),
		activeBridges: make(map[string]*DapBridge),
	}
}

// RegisterSession creates and registers a new bridge session.
// The token parameter should be the IDE session token (reused for bridge authentication).
// Returns the created session.
func (m *BridgeManager) RegisterSession(sessionID string, token string) (*BridgeSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[sessionID]; exists {
		return nil, ErrBridgeSessionAlreadyExists
	}

	session := &BridgeSession{
		ID:        sessionID,
		Token:     token,
		State:     BridgeSessionStateCreated,
		CreatedAt: time.Now(),
	}

	m.sessions[sessionID] = session
	m.log.Info("Registered bridge session", "sessionID", sessionID)
	return session, nil
}

// SocketPath returns the path to the Unix socket.
// This is only available after Start() has been called, as the socket path
// includes a random suffix generated during listener creation.
func (m *BridgeManager) SocketPath() string {
	if m.listener == nil {
		return ""
	}
	return m.listener.SocketPath()
}

// Ready returns a channel that is closed when the socket is ready to accept connections.
func (m *BridgeManager) Ready() <-chan struct{} {
	return m.readyCh
}

// Start begins listening on the Unix socket and accepting connections.
// This method blocks until the context is cancelled.
// Connections are handled in separate goroutines.
func (m *BridgeManager) Start(ctx context.Context) error {
	// Create the Unix socket listener
	var listenerErr error
	m.listener, listenerErr = networking.NewSecureSocketListener(m.socketDir, m.socketPrefix)
	if listenerErr != nil {
		return fmt.Errorf("failed to create socket listener: %w", listenerErr)
	}
	defer m.listener.Close()

	m.log.Info("Bridge manager listening", "socketPath", m.listener.SocketPath())

	// Signal that we're ready to accept connections
	m.readyOnce.Do(func() {
		close(m.readyCh)
	})

	// Accept connections in a loop
	for {
		select {
		case <-ctx.Done():
			m.log.V(1).Info("Bridge manager shutting down")
			return ctx.Err()
		default:
		}

		// Accept the next connection
		conn, acceptErr := m.listener.Accept()
		if acceptErr != nil {
			// Check if context was cancelled
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			m.log.Error(acceptErr, "Failed to accept connection")
			continue
		}

		// Handle the connection in a separate goroutine
		go m.handleConnection(ctx, conn)
	}
}

// validateHandshake validates a handshake request against registered sessions.
// Returns the session if validation succeeds.
func (m *BridgeManager) validateHandshake(sessionID, token string) (*BridgeSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, exists := m.sessions[sessionID]
	if !exists {
		return nil, ErrBridgeSessionNotFound
	}

	if session.Token != token {
		return nil, ErrBridgeSessionInvalidToken
	}

	return session, nil
}

// markSessionConnected marks a session as having an active connection.
// Returns an error if the session is not found or already has a connection.
func (m *BridgeManager) markSessionConnected(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, exists := m.sessions[sessionID]
	if !exists {
		return ErrBridgeSessionNotFound
	}

	if session.Connected {
		return fmt.Errorf("%w: session %s", ErrBridgeSessionAlreadyConnected, sessionID)
	}

	session.Connected = true
	m.log.V(1).Info("Marked session as connected", "sessionID", sessionID)
	return nil
}

// markSessionDisconnected resets the connected flag for a session.
// This is used to roll back markSessionConnected if later handshake steps fail.
// It is a no-op if the session does not exist.
func (m *BridgeManager) markSessionDisconnected(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if session, exists := m.sessions[sessionID]; exists {
		session.Connected = false
		m.log.V(1).Info("Reset session connected state", "sessionID", sessionID)
	}
}

// IsSessionConnected returns whether the given session has an active connection.
// Returns false if the session does not exist.
func (m *BridgeManager) IsSessionConnected(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, exists := m.sessions[sessionID]
	if !exists {
		return false
	}
	return session.Connected
}

// updateSessionState updates the state of a session.
func (m *BridgeManager) updateSessionState(sessionID string, state BridgeSessionState, errorMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, exists := m.sessions[sessionID]
	if !exists {
		return ErrBridgeSessionNotFound
	}

	oldState := session.State
	session.State = state
	session.Error = errorMsg

	m.log.V(1).Info("Bridge session state changed",
		"sessionID", sessionID,
		"oldState", oldState.String(),
		"newState", state.String())

	return nil
}

// handleConnection processes a single incoming connection.
func (m *BridgeManager) handleConnection(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error(fmt.Errorf("panic: %v", r), "Panic in connection handler")
			conn.Close()
		}
	}()

	log := m.log.WithValues("remoteAddr", conn.RemoteAddr())
	log.V(1).Info("Accepted connection")

	// Set handshake timeout
	timeout := m.config.HandshakeTimeout
	if timeout == 0 {
		timeout = DefaultHandshakeTimeout
	}
	if deadlineErr := conn.SetDeadline(time.Now().Add(timeout)); deadlineErr != nil {
		log.Error(deadlineErr, "Failed to set handshake deadline")
		conn.Close()
		return
	}

	// Read the handshake request
	reader := NewHandshakeReader(conn)
	writer := NewHandshakeWriter(conn)

	req, readErr := reader.ReadRequest()
	if readErr != nil {
		log.Error(readErr, "Failed to read handshake request")
		conn.Close()
		return
	}

	log = log.WithValues("sessionID", req.SessionID)
	log.V(1).Info("Received handshake request")

	// Validate token and session
	session, validateErr := m.validateHandshake(req.SessionID, req.Token)
	if validateErr != nil {
		log.Error(validateErr, "Handshake validation failed")
		resp := &HandshakeResponse{
			Success: false,
			Error:   validateErr.Error(),
		}
		_ = writer.WriteResponse(resp)
		conn.Close()
		return
	}

	// Check if adapter config is provided in handshake
	if req.DebugAdapterConfig == nil {
		log.Error(nil, "Handshake missing debug adapter configuration")
		resp := &HandshakeResponse{
			Success: false,
			Error:   "debug adapter configuration is required",
		}
		_ = writer.WriteResponse(resp)
		conn.Close()
		return
	}

	// Try to mark the session as connected (prevents duplicate connections)
	markErr := m.markSessionConnected(req.SessionID)
	if markErr != nil {
		log.Error(markErr, "Failed to mark session as connected")
		resp := &HandshakeResponse{
			Success: false,
			Error:   markErr.Error(),
		}
		_ = writer.WriteResponse(resp)
		conn.Close()
		return
	}

	// If anything fails between marking connected and handing off to runBridge,
	// roll back the connected state so the session can be retried.
	handedOff := false
	defer func() {
		if !handedOff {
			m.markSessionDisconnected(req.SessionID)
		}
	}()

	// Send success response
	resp := &HandshakeResponse{Success: true}
	if writeErr := writer.WriteResponse(resp); writeErr != nil {
		log.Error(writeErr, "Failed to send handshake response")
		conn.Close()
		return
	}

	// Clear the deadline for normal operation
	if deadlineErr := conn.SetDeadline(time.Time{}); deadlineErr != nil {
		log.Error(deadlineErr, "Failed to clear handshake deadline")
		conn.Close()
		return
	}

	log.Info("Handshake successful, starting bridge")

	// Disarm the rollback—runBridge now owns the session
	handedOff = true

	// Create and run the bridge
	m.runBridge(ctx, conn, session, req.RunID, req.DebugAdapterConfig, log)
}

// runBridge creates and runs a DapBridge for the given connection and session.
func (m *BridgeManager) runBridge(
	ctx context.Context,
	conn net.Conn,
	session *BridgeSession,
	runID string,
	adapterConfig *DebugAdapterConfig,
	log logr.Logger,
) {
	// Create the bridge configuration
	bridgeConfig := BridgeConfig{
		SessionID:     session.ID,
		AdapterConfig: adapterConfig,
		Executor:      m.executor,
		Logger:        log.WithName("DapBridge"),
	}

	// Resolve output handlers via the connection callback if configured
	if m.config.ConnectionHandler != nil {
		outputHandler, stdoutWriter, stderrWriter := m.config.ConnectionHandler(session.ID, runID)
		bridgeConfig.OutputHandler = outputHandler
		bridgeConfig.StdoutWriter = stdoutWriter
		bridgeConfig.StderrWriter = stderrWriter
	}

	// Create the bridge
	bridge := NewDapBridge(bridgeConfig)

	// Track active bridge
	m.mu.Lock()
	m.activeBridges[session.ID] = bridge
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.activeBridges, session.ID)
		m.mu.Unlock()
	}()

	// Update session state
	_ = m.updateSessionState(session.ID, BridgeSessionStateConnected, "")

	// Run the bridge with the already-connected IDE connection
	bridgeErr := bridge.RunWithConnection(ctx, conn)
	if bridgeErr != nil && !errors.Is(bridgeErr, context.Canceled) {
		log.Error(bridgeErr, "Bridge terminated with error")
		_ = m.updateSessionState(session.ID, BridgeSessionStateError, bridgeErr.Error())
	} else {
		_ = m.updateSessionState(session.ID, BridgeSessionStateTerminated, "")
	}
}
