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
	"github.com/microsoft/dcp/pkg/resiliency"
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

	// ParentSessionID is the ID of the parent session for child sessions.
	// Empty for primary (root) sessions.
	ParentSessionID string

	// AdapterAddress is the TCP address (host:port) of the adapter for TCP modes.
	// For child sessions, this is inherited from the parent and used to create
	// a new connection to the same adapter instance.
	AdapterAddress string

	// AdapterConfig is the debug adapter configuration from the parent session.
	// For child sessions with stdio adapters, this is used to launch a new
	// adapter instance with the same binary and arguments.
	AdapterConfig *DebugAdapterConfig

	// cancelFunc cancels the context for this session's bridge.
	// Used by parent sessions to cascade termination to children.
	cancelFunc context.CancelFunc
}

// Error constants for session management.
var (
	ErrBridgeSessionNotFound         = errors.New("bridge session not found")
	ErrBridgeSessionAlreadyExists    = errors.New("bridge session already exists")
	ErrBridgeSessionInvalidToken     = errors.New("invalid session token")
	ErrBridgeSessionAlreadyConnected = errors.New("session already connected")
	ErrBridgeSocketNotReady          = errors.New("bridge socket is not ready")
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

// BridgeTerminationHandler is called when a bridge session terminates, either because
// the debug adapter exited or because of an error. This acts as a safety net to ensure
// the resource transitions to a stopped state even if the IDE does not send a
// sessionTerminated notification.
//
// sessionID is the bridge session identifier (typically the Executable UID).
// runID is the IDE run session identifier provided during the handshake.
// exitCode is the exit code captured from the adapter's ExitedEvent, or nil if unavailable.
// err is non-nil if the bridge terminated due to an error.
type BridgeTerminationHandler func(sessionID string, runID string, exitCode *int32, err error)

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

	// HandshakeTimeout is the timeout for reading the handshake from a connection.
	// If zero, defaults to DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration

	// ConnectionHandler is called when a bridge connection is established to resolve
	// the OutputHandler and stdout/stderr writers for the session. If nil, output
	// from debug sessions will not be captured to executable log files.
	ConnectionHandler BridgeConnectionHandler

	// TerminationHandler is called when a bridge session terminates. This provides
	// a fallback mechanism to ensure the resource transitions to a stopped state
	// even if the IDE does not send a sessionTerminated notification. If nil,
	// the caller relies entirely on IDE notifications for session lifecycle.
	TerminationHandler BridgeTerminationHandler
}

// BridgeManager manages DAP bridge sessions and a shared Unix socket for IDE connections.
// It accepts incoming connections, performs handshake validation, and dispatches
// connections to the appropriate bridge sessions.
type BridgeManager struct {
	config   BridgeManagerConfig
	listener *networking.PrivateUnixSocketListener
	log      logr.Logger
	executor process.Executor

	// Socket configuration
	socketDir    string
	socketPrefix string
	readyCh      chan struct{}
	readyOnce    *sync.Once

	// listenerCh is closed by Start() after the listener field has been set
	// (whether successfully or not). SocketPath() blocks on this channel so
	// that it never observes the listener before Start() has initialised it.
	listenerCh   chan struct{}
	listenerOnce *sync.Once

	// mu protects sessions and activeBridges.
	mu            *sync.Mutex
	sessions      map[string]*BridgeSession
	activeBridges map[string]*DapBridge
}

// NewBridgeManager creates a new BridgeManager with the given configuration.
func NewBridgeManager(config BridgeManagerConfig, log logr.Logger) *BridgeManager {
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
		readyOnce:     &sync.Once{},
		listenerCh:    make(chan struct{}),
		listenerOnce:  &sync.Once{},
		mu:            &sync.Mutex{},
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

// RegisterChildSession registers a child debug session that is associated with a parent session.
// The child session inherits the parent's authentication token, adapter address, and adapter config.
// This is called by the bridge when handling a startDebugging reverse request.
func (m *BridgeManager) RegisterChildSession(parentSessionID, childSessionID string) (*BridgeSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	parent, parentExists := m.sessions[parentSessionID]
	if !parentExists {
		return nil, fmt.Errorf("parent session %s: %w", parentSessionID, ErrBridgeSessionNotFound)
	}

	if _, childExists := m.sessions[childSessionID]; childExists {
		return nil, ErrBridgeSessionAlreadyExists
	}

	// Determine adapter address: inherit from parent, or from the active bridge's adapter.
	adapterAddress := parent.AdapterAddress
	if adapterAddress == "" {
		if bridge, ok := m.activeBridges[parentSessionID]; ok && bridge.adapter != nil {
			adapterAddress = bridge.adapter.Address
		}
	}

	child := &BridgeSession{
		ID:              childSessionID,
		Token:           parent.Token,
		State:           BridgeSessionStateCreated,
		CreatedAt:       time.Now(),
		ParentSessionID: parentSessionID,
		AdapterAddress:  adapterAddress,
		AdapterConfig:   parent.AdapterConfig,
	}

	m.sessions[childSessionID] = child
	m.log.Info("Registered child bridge session",
		"childSessionID", childSessionID,
		"parentSessionID", parentSessionID,
		"adapterAddress", adapterAddress)
	return child, nil
}

// SocketPath returns the path to the Unix socket.
// It blocks until Start() has finished initialising the listener or ctx is cancelled.
func (m *BridgeManager) SocketPath(ctx context.Context) (string, error) {
	select {
	case <-m.listenerCh:
		// Start() has set the listener field.
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for bridge socket: %w", ctx.Err())
	}

	if m.listener == nil {
		return "", ErrBridgeSocketNotReady
	}
	return m.listener.SocketPath(), nil
}

// Ready returns a channel that is closed when the socket is ready to accept connections.
func (m *BridgeManager) Ready() <-chan struct{} {
	return m.readyCh
}

// Run begins listening on the Unix socket and accepting connections.
// This method blocks until the context is cancelled.
// Connections are handled in separate goroutines.
func (m *BridgeManager) Run(ctx context.Context) error {
	// Create the Unix socket listener
	var listenerErr error
	m.listener, listenerErr = networking.NewPrivateUnixSocketListener(m.socketDir, m.socketPrefix)

	// Signal that the listener field has been set so that SocketPath() can proceed.
	m.listenerOnce.Do(func() { close(m.listenerCh) })

	if listenerErr != nil {
		return fmt.Errorf("failed to create socket listener: %w", listenerErr)
	}
	defer m.listener.Close()

	m.log.Info("Bridge manager listening", "socketPath", m.listener.SocketPath())

	// Close the listener when the context is cancelled so that Accept() unblocks.
	// PrivateUnixSocketListener.Close() is idempotent, so the deferred Close above
	// is still safe.
	stopCloseListener := context.AfterFunc(ctx, func() {
		_ = m.listener.Close()
	})
	defer stopCloseListener()

	// Signal that we're ready to accept connections
	m.readyOnce.Do(func() {
		close(m.readyCh)
	})

	// Accept connections in a loop
	for {
		// Accept the next connection
		conn, acceptErr := m.listener.Accept()
		if acceptErr != nil {
			// Check if context was cancelled (listener was closed by context.AfterFunc above).
			if ctxErr := ctx.Err(); ctxErr != nil {
				m.log.V(1).Info("Bridge manager shutting down")
				return ctxErr
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
		panicErr := resiliency.MakePanicError(recover(), m.log)
		if panicErr != nil {
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

	// Check if adapter config is provided in handshake.
	// Child sessions (those with a parent) do not need adapter config in the handshake
	// because they inherit connection info from the parent.
	isChildSession := session.ParentSessionID != ""
	if req.DebugAdapterConfig == nil && !isChildSession {
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
	// Create a cancellable context for this session so parent cleanup can
	// cascade termination to child sessions.
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	// Store the cancel function on the session for parent-child lifecycle management.
	m.mu.Lock()
	session.cancelFunc = sessionCancel
	// For primary sessions, store the adapter config so child sessions can inherit it.
	if session.ParentSessionID == "" && adapterConfig != nil {
		session.AdapterConfig = adapterConfig
	}
	m.mu.Unlock()

	// Create the bridge configuration
	bridgeConfig := BridgeConfig{
		SessionID:     session.ID,
		AdapterConfig: adapterConfig,
		Executor:      m.executor,
		Logger:        log.WithName("DapBridge"),
	}

	// Set up the StartDebuggingHandler so the bridge can register child sessions.
	bridgeConfig.StartDebuggingHandler = func(parentSessionID string, configuration map[string]any, request string) (string, error) {
		return m.handleStartDebugging(parentSessionID, configuration, request)
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
		// Cancel all child sessions when this session terminates
		m.cancelChildSessions(session.ID)

		m.mu.Lock()
		delete(m.activeBridges, session.ID)
		m.mu.Unlock()
	}()

	// Update session state
	_ = m.updateSessionState(session.ID, BridgeSessionStateConnected, "")

	// Run the bridge. For child sessions, connect to the existing adapter
	// instead of launching a new one.
	var bridgeErr error
	isChildSession := session.ParentSessionID != ""
	if isChildSession {
		bridgeErr = m.runChildBridge(sessionCtx, bridge, conn, session, log)
	} else {
		bridgeErr = bridge.RunWithConnection(sessionCtx, conn)
	}

	// Update session state based on the result
	if bridgeErr != nil && !isExpectedShutdownErr(bridgeErr) {
		log.Error(bridgeErr, "Bridge terminated with error")
		_ = m.updateSessionState(session.ID, BridgeSessionStateError, bridgeErr.Error())
	} else {
		_ = m.updateSessionState(session.ID, BridgeSessionStateTerminated, "")
	}

	// Notify the termination handler so the resource can transition to stopped.
	// This acts as a safety net in case the IDE does not send a sessionTerminated
	// notification. The handler must be idempotent.
	if m.config.TerminationHandler != nil {
		var termErr error
		if bridgeErr != nil && !isExpectedShutdownErr(bridgeErr) {
			termErr = bridgeErr
		}
		m.config.TerminationHandler(session.ID, runID, bridge.ExitCode(), termErr)
	}
}

// runChildBridge runs a bridge for a child session by connecting to an existing adapter.
// For TCP adapters, it creates a new TCP connection to the parent's adapter address.
// For stdio adapters, it launches a new adapter instance with the parent's config.
func (m *BridgeManager) runChildBridge(
	ctx context.Context,
	bridge *DapBridge,
	conn net.Conn,
	session *BridgeSession,
	log logr.Logger,
) error {
	adapterMode := DebugAdapterModeStdio
	if session.AdapterConfig != nil {
		adapterMode = session.AdapterConfig.EffectiveMode()
	}

	switch adapterMode {
	case DebugAdapterModeTCPCallback, DebugAdapterModeTCPConnect:
		if session.AdapterAddress == "" {
			bridge.sendErrorToIDE("No adapter address available for child session")
			return errors.New("no adapter address available for child TCP session")
		}
		log.Info("Connecting child session to existing adapter",
			"adapterAddress", session.AdapterAddress)
		return bridge.runWithConnectionAndAdapter(ctx, conn, session.AdapterAddress)

	default:
		// Stdio mode: launch a new adapter instance with the parent's config
		if session.AdapterConfig == nil {
			bridge.sendErrorToIDE("No adapter config available for child stdio session")
			return errors.New("no adapter config available for child stdio session")
		}
		log.Info("Launching new adapter instance for child stdio session")
		return bridge.runWithConnectionAndConfig(ctx, conn, session.AdapterConfig)
	}
}

// handleStartDebugging is called by the bridge when it receives a startDebugging
// reverse request from the adapter. It registers a child session.
func (m *BridgeManager) handleStartDebugging(parentSessionID string, _ map[string]any, _ string) (string, error) {
	// Generate a unique child session ID
	m.mu.Lock()
	parentSession, exists := m.sessions[parentSessionID]
	if !exists {
		m.mu.Unlock()
		return "", fmt.Errorf("parent session %s: %w", parentSessionID, ErrBridgeSessionNotFound)
	}

	// Use a counter-based child session ID
	childCount := 0
	for _, s := range m.sessions {
		if s.ParentSessionID == parentSessionID {
			childCount++
		}
	}
	childSessionID := fmt.Sprintf("%s:%d", parentSessionID, childCount)

	// Determine adapter address from the active parent bridge
	adapterAddress := parentSession.AdapterAddress
	if adapterAddress == "" {
		if bridge, ok := m.activeBridges[parentSessionID]; ok && bridge.adapter != nil {
			adapterAddress = bridge.adapter.Address
		}
	}
	m.mu.Unlock()

	child, regErr := m.RegisterChildSession(parentSessionID, childSessionID)
	if regErr != nil {
		return "", fmt.Errorf("failed to register child session: %w", regErr)
	}

	// Update the adapter address in case it was resolved from the active bridge
	if child.AdapterAddress == "" && adapterAddress != "" {
		m.mu.Lock()
		child.AdapterAddress = adapterAddress
		m.mu.Unlock()
	}

	return childSessionID, nil
}

// cancelChildSessions cancels all child sessions of the given parent session.
func (m *BridgeManager) cancelChildSessions(parentSessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, session := range m.sessions {
		if session.ParentSessionID == parentSessionID && session.cancelFunc != nil {
			m.log.Info("Cancelling child session due to parent termination",
				"childSessionID", session.ID,
				"parentSessionID", parentSessionID)
			session.cancelFunc()
		}
	}
}
