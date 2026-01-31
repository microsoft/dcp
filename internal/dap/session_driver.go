/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-dap"
	"github.com/google/uuid"
)

// SessionDriver orchestrates the interaction between a DAP proxy and a gRPC control client.
// It manages the lifecycle of both components and provides the message callbacks that
// connect the proxy to the gRPC channel.
type SessionDriver struct {
	proxy  *Proxy
	client *ControlClient
	log    logr.Logger

	// currentStatus tracks the inferred debug session status
	statusMu      sync.Mutex
	currentStatus DebugSessionStatus
}

// NewSessionDriver creates a new session driver.
func NewSessionDriver(proxy *Proxy, client *ControlClient, log logr.Logger) *SessionDriver {
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	return &SessionDriver{
		proxy:         proxy,
		client:        client,
		log:           log,
		currentStatus: DebugSessionStatusConnecting,
	}
}

// Run starts the session driver and blocks until the session ends.
// It establishes the gRPC connection, starts the proxy with callbacks, and handles
// message routing between the proxy and gRPC channel.
//
// The context controls the lifetime of the session. Cancelling the context will
// terminate both the proxy and gRPC connection.
//
// Returns an aggregated error if any component fails. Context errors are filtered
// if they are redundant (i.e., caused by intentional shutdown).
func (d *SessionDriver) Run(ctx context.Context) error {
	// Connect to control server
	connectErr := d.client.Connect(ctx)
	if connectErr != nil {
		return connectErr
	}

	// Create proxy context that we can cancel independently
	proxyCtx, proxyCancel := context.WithCancel(ctx)
	defer proxyCancel()

	// Build callbacks
	upstreamCallback := d.buildUpstreamCallback()
	downstreamCallback := d.buildDownstreamCallback(proxyCtx)

	// Start proxy in a goroutine
	var proxyErr error
	var proxyWg sync.WaitGroup
	proxyWg.Add(1)
	go func() {
		defer proxyWg.Done()
		proxyErr = d.proxy.StartWithCallbacks(proxyCtx, upstreamCallback, downstreamCallback)
	}()

	// Start virtual request handler
	go d.handleVirtualRequests(proxyCtx)

	// Wait for termination signal
	select {
	case <-ctx.Done():
		d.log.Info("Session driver context cancelled")
	case <-d.client.Terminated():
		d.log.Info("gRPC connection terminated", "reason", d.client.TerminateReason())
	}

	// Shutdown sequence: proxy first, then client
	proxyCancel()
	proxyWg.Wait()

	clientErr := d.client.Close()

	// Filter and aggregate errors
	proxyErr = filterContextError(proxyErr, ctx, d.log)
	clientErr = filterContextError(clientErr, ctx, d.log)

	return errors.Join(proxyErr, clientErr)
}

// buildUpstreamCallback creates the callback for messages from the IDE.
func (d *SessionDriver) buildUpstreamCallback() MessageCallback {
	return func(msg dap.Message) CallbackResult {
		switch req := msg.(type) {
		case *dap.InitializeRequest:
			// Force support for runInTerminal so we can intercept it
			req.Arguments.SupportsRunInTerminalRequest = true
			return ForwardModified(req)

		default:
			return ForwardUnchanged()
		}
	}
}

// buildDownstreamCallback creates the callback for messages from the debug adapter.
func (d *SessionDriver) buildDownstreamCallback(ctx context.Context) MessageCallback {
	return func(msg dap.Message) CallbackResult {
		switch m := msg.(type) {
		case *dap.InitializeResponse:
			d.updateStatus(DebugSessionStatusInitializing)
			d.sendEventToServer(msg)
			return ForwardUnchanged()

		case *dap.ConfigurationDoneResponse:
			d.updateStatus(DebugSessionStatusAttached)
			d.sendEventToServer(msg)
			return ForwardUnchanged()

		case *dap.StoppedEvent:
			d.updateStatus(DebugSessionStatusStopped)
			d.sendEventToServer(msg)
			return ForwardUnchanged()

		case *dap.ContinuedEvent:
			d.updateStatus(DebugSessionStatusAttached)
			d.sendEventToServer(msg)
			return ForwardUnchanged()

		case *dap.TerminatedEvent:
			d.updateStatus(DebugSessionStatusTerminated)
			d.sendEventToServer(msg)
			return ForwardUnchanged()

		case *dap.RunInTerminalRequest:
			// Handle runInTerminal by forwarding to gRPC server
			return d.handleRunInTerminal(ctx, m)

		case dap.EventMessage:
			// Forward all other events to server
			d.sendEventToServer(msg)
			return ForwardUnchanged()

		default:
			return ForwardUnchanged()
		}
	}
}

// handleRunInTerminal processes a RunInTerminal request from the debug adapter.
func (d *SessionDriver) handleRunInTerminal(ctx context.Context, req *dap.RunInTerminalRequest) CallbackResult {
	d.log.Info("Handling RunInTerminal request",
		"kind", req.Arguments.Kind,
		"title", req.Arguments.Title,
		"cwd", req.Arguments.Cwd)

	// Create response channel
	respChan := make(chan AsyncResponse, 1)

	// Send request to server in a goroutine
	go func() {
		defer close(respChan)

		rtiReq := RunInTerminalRequestMsg{
			ID:    uuid.New().String(),
			Kind:  req.Arguments.Kind,
			Title: req.Arguments.Title,
			Cwd:   req.Arguments.Cwd,
			Args:  req.Arguments.Args,
			Env:   make(map[string]string),
		}

		// Copy environment variables
		if req.Arguments.Env != nil {
			for k, v := range req.Arguments.Env {
				if strVal, ok := v.(string); ok {
					rtiReq.Env[k] = strVal
				}
			}
		}

		processID, shellProcessID, rtiErr := d.client.SendRunInTerminalRequest(ctx, rtiReq)

		var response *dap.RunInTerminalResponse
		if rtiErr != nil {
			d.log.Error(rtiErr, "RunInTerminal request failed")
			response = &dap.RunInTerminalResponse{
				Response: dap.Response{
					ProtocolMessage: dap.ProtocolMessage{
						Type: "response",
					},
					Command:    "runInTerminal",
					RequestSeq: req.Seq,
					Success:    false,
					Message:    rtiErr.Error(),
				},
			}
		} else {
			response = &dap.RunInTerminalResponse{
				Response: dap.Response{
					ProtocolMessage: dap.ProtocolMessage{
						Type: "response",
					},
					Command:    "runInTerminal",
					RequestSeq: req.Seq,
					Success:    true,
				},
				Body: dap.RunInTerminalResponseBody{
					ProcessId:      int(processID),
					ShellProcessId: int(shellProcessID),
				},
			}
		}

		select {
		case respChan <- AsyncResponse{Response: response}:
		case <-ctx.Done():
		}
	}()

	return SuppressWithAsyncResponse(respChan)
}

// handleVirtualRequests processes virtual requests from the gRPC server.
func (d *SessionDriver) handleVirtualRequests(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-d.client.VirtualRequests():
			if !ok {
				return
			}
			d.processVirtualRequest(ctx, req)
		}
	}
}

// processVirtualRequest sends a virtual request to the debug adapter and returns the response.
func (d *SessionDriver) processVirtualRequest(ctx context.Context, req VirtualRequest) {
	d.log.V(1).Info("Processing virtual request", "requestId", req.ID)

	// Parse the DAP request
	dapMsg, parseErr := d.parseDAPMessage(req.Payload)
	if parseErr != nil {
		d.log.Error(parseErr, "Failed to parse virtual request")
		sendErr := d.client.SendResponse(req.ID, nil, parseErr)
		if sendErr != nil {
			d.log.Error(sendErr, "Failed to send error response")
		}
		return
	}

	// Create timeout context if specified
	reqCtx := ctx
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}

	// Send request to debug adapter
	response, sendReqErr := d.proxy.SendRequest(reqCtx, dapMsg)
	if sendReqErr != nil {
		d.log.Error(sendReqErr, "Virtual request failed", "requestId", req.ID)
		respErr := d.client.SendResponse(req.ID, nil, sendReqErr)
		if respErr != nil {
			d.log.Error(respErr, "Failed to send error response")
		}
		return
	}

	// Serialize response
	respPayload, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		d.log.Error(marshalErr, "Failed to serialize response")
		respErr := d.client.SendResponse(req.ID, nil, marshalErr)
		if respErr != nil {
			d.log.Error(respErr, "Failed to send error response")
		}
		return
	}

	// Send response to server
	respErr := d.client.SendResponse(req.ID, respPayload, nil)
	if respErr != nil {
		d.log.Error(respErr, "Failed to send response", "requestId", req.ID)
	}
}

// parseDAPMessage parses a JSON-encoded DAP message.
func (d *SessionDriver) parseDAPMessage(payload []byte) (dap.Message, error) {
	// First decode to get the message type
	var base struct {
		Type    string `json:"type"`
		Command string `json:"command,omitempty"`
		Event   string `json:"event,omitempty"`
	}
	if err := json.Unmarshal(payload, &base); err != nil {
		return nil, fmt.Errorf("failed to parse message type: %w", err)
	}

	// Use the DAP library's decoding if available, otherwise just unmarshal
	// For now, we'll use a simple approach
	msg, decodeErr := dap.DecodeProtocolMessage(payload)
	if decodeErr != nil {
		return nil, fmt.Errorf("failed to decode DAP message: %w", decodeErr)
	}

	return msg, nil
}

// sendEventToServer forwards a DAP event to the gRPC server.
func (d *SessionDriver) sendEventToServer(msg dap.Message) {
	payload, marshalErr := json.Marshal(msg)
	if marshalErr != nil {
		d.log.Error(marshalErr, "Failed to serialize event")
		return
	}

	sendErr := d.client.SendEvent(payload)
	if sendErr != nil {
		d.log.Error(sendErr, "Failed to send event to server")
	}
}

// updateStatus updates the current session status and notifies the server.
func (d *SessionDriver) updateStatus(status DebugSessionStatus) {
	d.statusMu.Lock()
	if d.currentStatus == status {
		d.statusMu.Unlock()
		return
	}
	d.currentStatus = status
	d.statusMu.Unlock()

	d.log.V(1).Info("Session status changed", "status", status.String())

	sendErr := d.client.SendStatusUpdate(status, "")
	if sendErr != nil {
		d.log.Error(sendErr, "Failed to send status update")
	}
}
