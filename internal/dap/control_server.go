/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/microsoft/dcp/internal/dap/proto"
	"github.com/microsoft/dcp/pkg/commonapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	// AuthorizationHeader is the metadata key for bearer token authentication.
	AuthorizationHeader = "authorization"

	// BearerPrefix is the prefix for bearer tokens in the authorization header.
	BearerPrefix = "Bearer "
)

// ControlServerConfig contains configuration for the DAP control server.
type ControlServerConfig struct {
	// Listener is the network listener for the gRPC server.
	// If nil, the server will create a listener on the specified address.
	Listener net.Listener

	// Address is the address to listen on if Listener is nil.
	Address string

	// TLSConfig is the TLS configuration for the server.
	// If nil, the server will use insecure connections.
	TLSConfig *tls.Config

	// BearerToken is the expected bearer token for authentication.
	// If empty, authentication is disabled.
	BearerToken string

	// Logger is the logger for the server.
	Logger logr.Logger

	// SessionMap is the shared session map for pre-registration.
	// If nil, a new SessionMap is created (for backward compatibility in tests).
	SessionMap *SessionMap

	// RunInTerminalHandler is called when a proxy sends a RunInTerminal request.
	// The handler should execute the command and return the result.
	RunInTerminalHandler func(ctx context.Context, key commonapi.NamespacedNameWithKind, req *proto.RunInTerminalRequest) *proto.RunInTerminalResponse

	// EventHandler is called when a proxy sends a DAP event.
	EventHandler func(key commonapi.NamespacedNameWithKind, payload []byte)

	// CapabilitiesHandler is called when a proxy sends debug adapter capabilities.
	CapabilitiesHandler func(key commonapi.NamespacedNameWithKind, capabilitiesJSON []byte)
}

// ControlServer is a gRPC server that manages DAP proxy sessions.
type ControlServer struct {
	proto.UnimplementedDapControlServer

	config   ControlServerConfig
	sessions *SessionMap
	server   *grpc.Server
	log      logr.Logger

	// activeStreams tracks active session streams for sending messages
	streamsMu sync.RWMutex
	streams   map[string]*sessionStream

	// pendingRequests tracks virtual requests awaiting responses
	pendingMu       sync.Mutex
	pendingRequests map[string]chan *proto.VirtualResponse
}

// sessionStream holds the stream and metadata for an active session.
type sessionStream struct {
	key        commonapi.NamespacedNameWithKind
	stream     grpc.BidiStreamingServer[proto.SessionMessage, proto.SessionMessage]
	sendMu     sync.Mutex
	ctx        context.Context
	cancelFunc context.CancelFunc
}

// NewControlServer creates a new DAP control server.
func NewControlServer(config ControlServerConfig) *ControlServer {
	log := config.Logger
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	sessions := config.SessionMap
	if sessions == nil {
		// Create a new SessionMap for backward compatibility (tests)
		sessions = NewSessionMap()
	}

	return &ControlServer{
		config:          config,
		sessions:        sessions,
		log:             log,
		streams:         make(map[string]*sessionStream),
		pendingRequests: make(map[string]chan *proto.VirtualResponse),
	}
}

// Start starts the gRPC server and blocks until the context is cancelled.
func (s *ControlServer) Start(ctx context.Context) error {
	listener := s.config.Listener
	if listener == nil {
		var listenErr error
		listener, listenErr = net.Listen("tcp", s.config.Address)
		if listenErr != nil {
			return fmt.Errorf("failed to listen: %w", listenErr)
		}
	}

	var opts []grpc.ServerOption
	if s.config.TLSConfig != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(s.config.TLSConfig)))
	}

	s.server = grpc.NewServer(opts...)
	proto.RegisterDapControlServer(s.server, s)

	errChan := make(chan error, 1)
	go func() {
		s.log.Info("Starting DAP control server", "address", listener.Addr().String())
		if serveErr := s.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			errChan <- serveErr
		}
		close(errChan)
	}()

	select {
	case <-ctx.Done():
		s.log.Info("Stopping DAP control server")
		s.server.GracefulStop()
		return ctx.Err()
	case serveErr := <-errChan:
		return serveErr
	}
}

// Stop stops the gRPC server gracefully.
func (s *ControlServer) Stop() {
	if s.server != nil {
		s.server.GracefulStop()
	}
}

// Sessions returns the session map for querying session state.
func (s *ControlServer) Sessions() *SessionMap {
	return s.sessions
}

// DebugSession implements the bidirectional streaming RPC for debug sessions.
func (s *ControlServer) DebugSession(stream grpc.BidiStreamingServer[proto.SessionMessage, proto.SessionMessage]) error {
	ctx := stream.Context()

	// Validate authentication
	if s.config.BearerToken != "" {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return status.Error(codes.Unauthenticated, "missing metadata")
		}

		authValues := md.Get(AuthorizationHeader)
		if len(authValues) == 0 {
			return status.Error(codes.Unauthenticated, "missing authorization header")
		}

		token := authValues[0]
		expectedToken := BearerPrefix + s.config.BearerToken
		if token != expectedToken {
			s.log.Info("Authentication failed: invalid token")
			return status.Error(codes.Unauthenticated, "invalid token")
		}
	}

	// Wait for handshake
	msg, recvErr := stream.Recv()
	if recvErr != nil {
		return fmt.Errorf("failed to receive handshake: %w", recvErr)
	}

	handshake := msg.GetHandshake()
	if handshake == nil {
		return status.Error(codes.InvalidArgument, "expected handshake message")
	}

	resourceKey := ToNamespacedNameWithKind(handshake.Resource)
	if resourceKey.Empty() {
		return status.Error(codes.InvalidArgument, "invalid resource identifier")
	}

	s.log.Info("Received handshake", "resource", resourceKey.String())

	// Create session context
	sessionCtx, sessionCancel := context.WithCancel(ctx)

	// Try to claim the session; if not registered, try parking
	var adapterConfig *DebugAdapterConfig
	claimErr := s.sessions.ClaimSession(resourceKey, sessionCancel)
	if claimErr != nil {
		if errors.Is(claimErr, ErrSessionNotPreRegistered) {
			// Check if session was rejected
			if reason, rejected := s.sessions.IsSessionRejected(resourceKey); rejected {
				sessionCancel()
				s.log.Info("Session rejected", "resource", resourceKey.String(), "reason", reason)
				sendRejectResponse(stream, reason, s.log)
				return status.Error(codes.FailedPrecondition, reason)
			}

			// Park the connection and wait for registration
			s.log.Info("Session not registered, parking connection", "resource", resourceKey.String())
			var parkErr error
			adapterConfig, parkErr = s.sessions.ParkConnection(ctx, resourceKey, DefaultParkingTimeout)
			if parkErr != nil {
				sessionCancel()
				s.log.Info("Session parking failed", "resource", resourceKey.String(), "error", parkErr)
				sendRejectResponse(stream, parkErr.Error(), s.log)
				return status.Error(codes.NotFound, parkErr.Error())
			}

			// Now try to claim the session again
			claimErr = s.sessions.ClaimSession(resourceKey, sessionCancel)
		}

		if claimErr != nil {
			sessionCancel()
			var errorMsg string
			var grpcCode codes.Code

			if errors.Is(claimErr, ErrSessionNotPreRegistered) {
				s.log.Info("Session rejected: not pre-registered", "resource", resourceKey.String())
				errorMsg = "session not pre-registered for this resource"
				grpcCode = codes.NotFound
			} else if errors.Is(claimErr, ErrSessionAlreadyClaimed) {
				s.log.Info("Session rejected: already claimed", "resource", resourceKey.String())
				errorMsg = "session already connected for this resource"
				grpcCode = codes.AlreadyExists
			} else {
				errorMsg = "failed to claim session"
				grpcCode = codes.Internal
			}

			sendRejectResponse(stream, errorMsg, s.log)
			return status.Error(grpcCode, errorMsg)
		}
	}

	defer func() {
		s.sessions.ReleaseSession(resourceKey)
		sessionCancel()
	}()

	// Get adapter config for this session (if not already from parking)
	if adapterConfig == nil {
		adapterConfig = s.sessions.GetAdapterConfig(resourceKey)
	}
	protoAdapterConfig := toProtoAdapterConfig(adapterConfig)

	// Send handshake response with adapter config
	sendErr := stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_HandshakeResponse{
			HandshakeResponse: &proto.HandshakeResponse{
				Success:       ptrBool(true),
				AdapterConfig: protoAdapterConfig,
			},
		},
	})
	if sendErr != nil {
		return fmt.Errorf("failed to send handshake response: %w", sendErr)
	}

	// Register stream for sending messages
	streamKey := resourceKey.String()
	ss := &sessionStream{
		key:        resourceKey,
		stream:     stream,
		ctx:        sessionCtx,
		cancelFunc: sessionCancel,
	}
	s.streamsMu.Lock()
	s.streams[streamKey] = ss
	s.streamsMu.Unlock()

	defer func() {
		s.streamsMu.Lock()
		delete(s.streams, streamKey)
		s.streamsMu.Unlock()
	}()

	s.log.Info("Session established", "resource", resourceKey.String())

	// Process incoming messages
	for {
		select {
		case <-sessionCtx.Done():
			s.log.Info("Session context cancelled", "resource", resourceKey.String())
			return nil
		default:
		}

		inMsg, inErr := stream.Recv()
		if inErr != nil {
			if errors.Is(inErr, io.EOF) {
				s.log.Info("Session stream closed by client", "resource", resourceKey.String())
				return nil
			}
			if sessionCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("failed to receive message: %w", inErr)
		}

		s.handleSessionMessage(sessionCtx, resourceKey, ss, inMsg)
	}
}

// handleSessionMessage processes an incoming message from a proxy.
func (s *ControlServer) handleSessionMessage(
	ctx context.Context,
	key commonapi.NamespacedNameWithKind,
	ss *sessionStream,
	msg *proto.SessionMessage,
) {
	switch m := msg.Message.(type) {
	case *proto.SessionMessage_VirtualResponse:
		s.handleVirtualResponse(m.VirtualResponse)

	case *proto.SessionMessage_Event:
		if s.config.EventHandler != nil {
			s.config.EventHandler(key, m.Event.Payload)
		}

	case *proto.SessionMessage_RunInTerminalRequest:
		s.handleRunInTerminalRequest(ctx, key, ss, m.RunInTerminalRequest)

	case *proto.SessionMessage_StatusUpdate:
		status := ToDebugSessionStatus(m.StatusUpdate.GetStatus())
		s.sessions.UpdateSessionStatus(key, status, m.StatusUpdate.GetError())
		s.log.V(1).Info("Session status updated", "resource", key.String(), "status", status.String())

	case *proto.SessionMessage_CapabilitiesUpdate:
		s.handleCapabilitiesUpdate(key, m.CapabilitiesUpdate)

	default:
		s.log.Info("Unexpected message type from proxy", "type", fmt.Sprintf("%T", msg.Message))
	}
}

// handleVirtualResponse processes a response to a virtual request.
func (s *ControlServer) handleVirtualResponse(resp *proto.VirtualResponse) {
	requestID := resp.GetRequestId()

	s.pendingMu.Lock()
	ch, exists := s.pendingRequests[requestID]
	if exists {
		delete(s.pendingRequests, requestID)
	}
	s.pendingMu.Unlock()

	if !exists {
		s.log.Info("Received response for unknown request", "requestId", requestID)
		return
	}

	select {
	case ch <- resp:
	default:
		s.log.Info("Response channel full, dropping response", "requestId", requestID)
	}
	close(ch)
}

// handleRunInTerminalRequest processes a RunInTerminal request from a proxy.
func (s *ControlServer) handleRunInTerminalRequest(
	ctx context.Context,
	key commonapi.NamespacedNameWithKind,
	ss *sessionStream,
	req *proto.RunInTerminalRequest,
) {
	s.log.Info("Received RunInTerminal request",
		"resource", key.String(),
		"requestId", req.GetRequestId(),
		"kind", req.GetKind(),
		"title", req.GetTitle())

	var resp *proto.RunInTerminalResponse
	if s.config.RunInTerminalHandler != nil {
		resp = s.config.RunInTerminalHandler(ctx, key, req)
	} else {
		// Default response if no handler configured
		resp = &proto.RunInTerminalResponse{
			RequestId: req.RequestId,
			Error:     ptrString("no RunInTerminal handler configured"),
		}
	}

	resp.RequestId = req.RequestId

	// Send response back to proxy
	ss.sendMu.Lock()
	defer ss.sendMu.Unlock()

	sendErr := ss.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_RunInTerminalResponse{
			RunInTerminalResponse: resp,
		},
	})
	if sendErr != nil {
		s.log.Error(sendErr, "Failed to send RunInTerminal response", "resource", key.String())
	}
}

// handleCapabilitiesUpdate processes a capabilities update from a proxy.
func (s *ControlServer) handleCapabilitiesUpdate(
	key commonapi.NamespacedNameWithKind,
	update *proto.CapabilitiesUpdate,
) {
	s.log.V(1).Info("Received capabilities update",
		"resource", key.String(),
		"size", len(update.GetCapabilitiesJson()))

	// Parse and store capabilities in session map
	capabilitiesJSON := update.GetCapabilitiesJson()
	if len(capabilitiesJSON) > 0 {
		var capabilities map[string]interface{}
		if err := json.Unmarshal(capabilitiesJSON, &capabilities); err == nil {
			s.sessions.SetCapabilities(key, capabilities)
		} else {
			s.log.Error(err, "Failed to parse capabilities JSON", "resource", key.String())
		}
	}

	// Call handler if configured
	if s.config.CapabilitiesHandler != nil {
		s.config.CapabilitiesHandler(key, capabilitiesJSON)
	}
}

// SendVirtualRequest sends a virtual DAP request to a connected proxy and waits for the response.
// The timeout specifies how long to wait for a response; zero means no timeout.
func (s *ControlServer) SendVirtualRequest(
	ctx context.Context,
	key commonapi.NamespacedNameWithKind,
	payload []byte,
	timeout time.Duration,
) ([]byte, error) {
	s.streamsMu.RLock()
	ss, exists := s.streams[key.String()]
	s.streamsMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("no active session for resource %s: %w", key.String(), ErrSessionRejected)
	}

	// Generate request ID
	requestID := uuid.New().String()

	// Create response channel
	respChan := make(chan *proto.VirtualResponse, 1)
	s.pendingMu.Lock()
	s.pendingRequests[requestID] = respChan
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pendingRequests, requestID)
		s.pendingMu.Unlock()
	}()

	// Send request
	ss.sendMu.Lock()
	sendErr := ss.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_VirtualRequest{
			VirtualRequest: &proto.VirtualRequest{
				RequestId: ptrString(requestID),
				Payload:   payload,
				TimeoutMs: ptrInt64(timeout.Milliseconds()),
			},
		},
	})
	ss.sendMu.Unlock()

	if sendErr != nil {
		return nil, fmt.Errorf("failed to send virtual request: %w", sendErr)
	}

	// Wait for response with timeout
	waitCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	select {
	case resp, ok := <-respChan:
		if !ok {
			return nil, ErrSessionTerminated
		}
		if resp.GetError() != "" {
			return nil, fmt.Errorf("virtual request failed: %s", resp.GetError())
		}
		return resp.Payload, nil
	case <-waitCtx.Done():
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrRequestTimeout
		}
		return nil, waitCtx.Err()
	case <-ss.ctx.Done():
		return nil, ErrSessionTerminated
	}
}

// TerminateSession terminates a debug session for the given resource.
func (s *ControlServer) TerminateSession(key commonapi.NamespacedNameWithKind, reason string) {
	s.streamsMu.RLock()
	ss, exists := s.streams[key.String()]
	s.streamsMu.RUnlock()

	if !exists {
		return
	}

	s.log.Info("Terminating session", "resource", key.String(), "reason", reason)

	// Send terminate message
	ss.sendMu.Lock()
	sendErr := ss.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_Terminate{
			Terminate: &proto.Terminate{
				Reason: ptrString(reason),
			},
		},
	})
	ss.sendMu.Unlock()

	if sendErr != nil {
		s.log.Error(sendErr, "Failed to send terminate message", "resource", key.String())
	}

	// Cancel session context to trigger cleanup
	s.sessions.TerminateSession(key)
}

// GetSessionStatus returns the current status of a debug session.
func (s *ControlServer) GetSessionStatus(key commonapi.NamespacedNameWithKind) *DebugSessionState {
	return s.sessions.GetSessionStatus(key)
}

// SessionEvents returns a channel that receives session lifecycle events.
func (s *ControlServer) SessionEvents() <-chan SessionEvent {
	return s.sessions.SessionEvents()
}

// sendRejectResponse sends a handshake rejection response on the stream.
func sendRejectResponse(stream grpc.BidiStreamingServer[proto.SessionMessage, proto.SessionMessage], errorMsg string, log logr.Logger) {
	sendErr := stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_HandshakeResponse{
			HandshakeResponse: &proto.HandshakeResponse{
				Success: ptrBool(false),
				Error:   ptrString(errorMsg),
			},
		},
	})
	if sendErr != nil {
		log.Error(sendErr, "Failed to send handshake rejection")
	}
}
