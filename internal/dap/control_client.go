/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/go-logr/logr"
	"github.com/microsoft/dcp/internal/dap/proto"
	"github.com/microsoft/dcp/pkg/commonapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ControlClientConfig contains configuration for connecting to a DAP control server.
type ControlClientConfig struct {
	// Endpoint is the gRPC server address.
	Endpoint string

	// PinnedCert is the server's certificate for TLS verification.
	// If nil, certificate verification uses the system roots.
	PinnedCert *x509.Certificate

	// BearerToken is the authentication token.
	BearerToken string

	// ResourceKey identifies the resource being debugged.
	ResourceKey commonapi.NamespacedNameWithKind

	// Logger is the logger for the client.
	Logger logr.Logger
}

// VirtualRequest represents a virtual DAP request from the server.
type VirtualRequest struct {
	// ID is the unique request identifier for correlating responses.
	ID string

	// Payload is the JSON-encoded DAP request message.
	Payload []byte

	// TimeoutMs is the timeout for the request in milliseconds.
	TimeoutMs int64
}

// RunInTerminalRequestMsg represents a RunInTerminal request message.
type RunInTerminalRequestMsg struct {
	// ID is the unique request identifier.
	ID string

	// Kind is the terminal kind: "integrated" or "external".
	Kind string

	// Title is the optional terminal title.
	Title string

	// Cwd is the working directory.
	Cwd string

	// Args are the command arguments.
	Args []string

	// Env are the environment variables.
	Env map[string]string
}

// ControlClient is a gRPC client for communicating with a DAP control server.
type ControlClient struct {
	config ControlClientConfig
	log    logr.Logger

	conn   *grpc.ClientConn
	stream grpc.BidiStreamingClient[proto.SessionMessage, proto.SessionMessage]

	// adapterConfig holds the debug adapter configuration received during handshake.
	adapterConfig *DebugAdapterConfig

	// Channels for incoming messages
	virtualRequests chan VirtualRequest
	terminatedChan  chan struct{}
	terminateReason string

	// pendingRTI tracks pending RunInTerminal requests
	rtiMu      sync.Mutex
	rtiPending map[string]chan *proto.RunInTerminalResponse

	// sendMu protects stream.Send calls
	sendMu sync.Mutex

	// ctx is the client context
	ctx    context.Context
	cancel context.CancelFunc

	// closed indicates the client has been closed
	closed   bool
	closedMu sync.Mutex
}

// NewControlClient creates a new DAP control client.
func NewControlClient(config ControlClientConfig) *ControlClient {
	log := config.Logger
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	return &ControlClient{
		config:          config,
		log:             log,
		virtualRequests: make(chan VirtualRequest, 10),
		terminatedChan:  make(chan struct{}),
		rtiPending:      make(map[string]chan *proto.RunInTerminalResponse),
	}
}

// Connect establishes a connection to the control server and performs the handshake.
func (c *ControlClient) Connect(ctx context.Context) error {
	c.closedMu.Lock()
	if c.closed {
		c.closedMu.Unlock()
		return ErrGRPCConnectionFailed
	}
	c.closedMu.Unlock()

	c.ctx, c.cancel = context.WithCancel(ctx)

	// Build dial options
	var opts []grpc.DialOption

	if c.config.PinnedCert != nil {
		// Use pinned certificate for verification
		certPool := x509.NewCertPool()
		certPool.AddCert(c.config.PinnedCert)
		tlsConfig := &tls.Config{
			RootCAs:    certPool,
			MinVersion: tls.VersionTLS12,
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		// Insecure connection (for development/testing)
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Connect to server
	var dialErr error
	c.conn, dialErr = grpc.NewClient(c.config.Endpoint, opts...)
	if dialErr != nil {
		return fmt.Errorf("%w: %v", ErrGRPCConnectionFailed, dialErr)
	}

	// Create stream with authentication metadata
	client := proto.NewDapControlClient(c.conn)

	streamCtx := c.ctx
	if c.config.BearerToken != "" {
		md := metadata.New(map[string]string{
			AuthorizationHeader: BearerPrefix + c.config.BearerToken,
		})
		streamCtx = metadata.NewOutgoingContext(c.ctx, md)
	}

	var streamErr error
	c.stream, streamErr = client.DebugSession(streamCtx)
	if streamErr != nil {
		c.conn.Close()
		return fmt.Errorf("%w: %v", ErrGRPCConnectionFailed, streamErr)
	}

	// Send handshake
	handshakeErr := c.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_Handshake{
			Handshake: &proto.Handshake{
				Resource: FromNamespacedNameWithKind(c.config.ResourceKey),
			},
		},
	})
	if handshakeErr != nil {
		c.conn.Close()
		return fmt.Errorf("%w: failed to send handshake: %v", ErrGRPCConnectionFailed, handshakeErr)
	}

	// Wait for handshake response
	resp, recvErr := c.stream.Recv()
	if recvErr != nil {
		c.conn.Close()
		return fmt.Errorf("%w: failed to receive handshake response: %v", ErrGRPCConnectionFailed, recvErr)
	}

	handshakeResp := resp.GetHandshakeResponse()
	if handshakeResp == nil {
		c.conn.Close()
		return fmt.Errorf("%w: expected handshake response", ErrGRPCConnectionFailed)
	}

	if !handshakeResp.GetSuccess() {
		c.conn.Close()
		return fmt.Errorf("%w: %s", ErrSessionRejected, handshakeResp.GetError())
	}

	// Extract adapter config from handshake response
	c.adapterConfig = FromProtoAdapterConfig(handshakeResp.GetAdapterConfig())
	if c.adapterConfig != nil {
		c.log.Info("Received adapter config",
			"args", c.adapterConfig.Args,
			"mode", c.adapterConfig.Mode.String())
	}

	c.log.Info("Connected to control server", "resource", c.config.ResourceKey.String())

	// Start receive loop
	go c.receiveLoop()

	return nil
}

// receiveLoop reads messages from the server and dispatches them to channels.
func (c *ControlClient) receiveLoop() {
	defer func() {
		c.closedMu.Lock()
		if !c.closed {
			c.closed = true
			close(c.terminatedChan)
		}
		c.closedMu.Unlock()
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		msg, recvErr := c.stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				c.log.Info("Server closed connection")
			} else if c.ctx.Err() == nil {
				c.log.Error(recvErr, "Error receiving message")
			}
			return
		}

		c.handleServerMessage(msg)
	}
}

// handleServerMessage processes a message from the server.
func (c *ControlClient) handleServerMessage(msg *proto.SessionMessage) {
	switch m := msg.Message.(type) {
	case *proto.SessionMessage_VirtualRequest:
		vr := m.VirtualRequest
		req := VirtualRequest{
			ID:        vr.GetRequestId(),
			Payload:   vr.GetPayload(),
			TimeoutMs: vr.GetTimeoutMs(),
		}
		select {
		case c.virtualRequests <- req:
		case <-c.ctx.Done():
		}

	case *proto.SessionMessage_RunInTerminalResponse:
		resp := m.RunInTerminalResponse
		requestID := resp.GetRequestId()

		c.rtiMu.Lock()
		ch, exists := c.rtiPending[requestID]
		if exists {
			delete(c.rtiPending, requestID)
		}
		c.rtiMu.Unlock()

		if exists {
			select {
			case ch <- resp:
			default:
			}
			close(ch)
		} else {
			c.log.Info("Received RunInTerminal response for unknown request",
				"requestId", requestID)
		}

	case *proto.SessionMessage_Terminate:
		c.log.Info("Server requested termination", "reason", m.Terminate.GetReason())
		c.terminateReason = m.Terminate.GetReason()
		c.cancel()

	default:
		c.log.Info("Unexpected message type from server", "type", fmt.Sprintf("%T", msg.Message))
	}
}

// GetAdapterConfig returns the debug adapter configuration received during handshake.
// Returns nil if no adapter config was provided.
func (c *ControlClient) GetAdapterConfig() *DebugAdapterConfig {
	return c.adapterConfig
}

// VirtualRequests returns a channel that receives virtual DAP requests from the server.
func (c *ControlClient) VirtualRequests() <-chan VirtualRequest {
	return c.virtualRequests
}

// SendResponse sends a response to a virtual request.
func (c *ControlClient) SendResponse(requestID string, payload []byte, err error) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	var errStr *string
	if err != nil {
		s := err.Error()
		errStr = &s
	}

	return c.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_VirtualResponse{
			VirtualResponse: &proto.VirtualResponse{
				RequestId: ptrString(requestID),
				Payload:   payload,
				Error:     errStr,
			},
		},
	})
}

// SendEvent sends a DAP event to the server.
func (c *ControlClient) SendEvent(payload []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	return c.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_Event{
			Event: &proto.Event{
				Payload: payload,
			},
		},
	})
}

// SendStatusUpdate sends a status update to the server.
func (c *ControlClient) SendStatusUpdate(status DebugSessionStatus, errorMsg string) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	return c.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_StatusUpdate{
			StatusUpdate: &proto.StatusUpdate{
				Status: FromDebugSessionStatus(status),
				Error:  ptrString(errorMsg),
			},
		},
	})
}

// SendCapabilities sends the debug adapter capabilities to the server.
func (c *ControlClient) SendCapabilities(capabilitiesJSON []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	return c.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_CapabilitiesUpdate{
			CapabilitiesUpdate: &proto.CapabilitiesUpdate{
				CapabilitiesJson: capabilitiesJSON,
			},
		},
	})
}

// SendRunInTerminalRequest sends a RunInTerminal request to the server and waits for the response.
func (c *ControlClient) SendRunInTerminalRequest(ctx context.Context, req RunInTerminalRequestMsg) (processID, shellProcessID int64, err error) {
	// Create response channel
	respChan := make(chan *proto.RunInTerminalResponse, 1)

	c.rtiMu.Lock()
	c.rtiPending[req.ID] = respChan
	c.rtiMu.Unlock()

	defer func() {
		c.rtiMu.Lock()
		delete(c.rtiPending, req.ID)
		c.rtiMu.Unlock()
	}()

	// Send request
	c.sendMu.Lock()
	sendErr := c.stream.Send(&proto.SessionMessage{
		Message: &proto.SessionMessage_RunInTerminalRequest{
			RunInTerminalRequest: &proto.RunInTerminalRequest{
				RequestId: ptrString(req.ID),
				Kind:      ptrString(req.Kind),
				Title:     ptrString(req.Title),
				Cwd:       ptrString(req.Cwd),
				Args:      req.Args,
				Env:       req.Env,
			},
		},
	})
	c.sendMu.Unlock()

	if sendErr != nil {
		return 0, 0, fmt.Errorf("failed to send RunInTerminal request: %w", sendErr)
	}

	// Wait for response
	select {
	case resp := <-respChan:
		if resp.GetError() != "" {
			return 0, 0, fmt.Errorf("RunInTerminal failed: %s", resp.GetError())
		}
		return resp.GetProcessId(), resp.GetShellProcessId(), nil
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	case <-c.terminatedChan:
		return 0, 0, ErrSessionTerminated
	}
}

// Terminated returns a channel that is closed when the connection is terminated.
func (c *ControlClient) Terminated() <-chan struct{} {
	return c.terminatedChan
}

// TerminateReason returns the reason for termination, if any.
func (c *ControlClient) TerminateReason() string {
	return c.terminateReason
}

// Close closes the client connection.
func (c *ControlClient) Close() error {
	c.closedMu.Lock()
	if c.closed {
		c.closedMu.Unlock()
		return nil
	}
	c.closed = true
	close(c.terminatedChan)
	c.closedMu.Unlock()

	if c.cancel != nil {
		c.cancel()
	}

	var closeErr error
	if c.stream != nil {
		closeErr = c.stream.CloseSend()
	}
	if c.conn != nil {
		connErr := c.conn.Close()
		if closeErr == nil {
			closeErr = connErr
		}
	}

	return closeErr
}
