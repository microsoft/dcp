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
	"github.com/microsoft/dcp/pkg/process"
)

// SessionDriverConfig holds the configuration for creating a SessionDriver.
type SessionDriverConfig struct {
	// UpstreamTransport is the connection to the IDE/client.
	UpstreamTransport Transport

	// ControlClient is the gRPC client for communicating with the control server.
	ControlClient *ControlClient

	// Executor is the process executor for managing debug adapter processes.
	// If nil, a new executor will be created.
	Executor process.Executor

	// Logger for session driver operations.
	Logger logr.Logger

	// ProxyConfig is optional configuration for the proxy.
	ProxyConfig ProxyConfig
}

// SessionDriver orchestrates the interaction between a DAP proxy and a gRPC control client.
// It manages the lifecycle of the debug adapter process, the proxy, and the gRPC connection.
type SessionDriver struct {
	upstreamTransport Transport
	client            *ControlClient
	executor          process.Executor
	proxyConfig       ProxyConfig
	log               logr.Logger

	// proxy is created during Run
	proxy *Proxy

	// currentStatus tracks the inferred debug session status
	statusMu      sync.Mutex
	currentStatus DebugSessionStatus
}

// NewSessionDriver creates a new session driver.
func NewSessionDriver(config SessionDriverConfig) *SessionDriver {
	log := config.Logger
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	executor := config.Executor
	if executor == nil {
		executor = process.NewOSExecutor(log)
	}

	return &SessionDriver{
		upstreamTransport: config.UpstreamTransport,
		client:            config.ControlClient,
		executor:          executor,
		proxyConfig:       config.ProxyConfig,
		log:               log,
		currentStatus:     DebugSessionStatusConnecting,
	}
}

// Run starts the session driver and blocks until the session ends.
// It establishes the gRPC connection, launches the debug adapter, creates the proxy,
// and handles message routing between components.
//
// The context controls the lifetime of the session. Cancelling the context will
// terminate the debug adapter process, proxy, and gRPC connection.
//
// Returns an aggregated error if any component fails. Context errors are filtered
// if they are redundant (i.e., caused by intentional shutdown).
func (d *SessionDriver) Run(ctx context.Context) error {
	// Connect to control server
	connectErr := d.client.Connect(ctx)
	if connectErr != nil {
		return connectErr
	}

	// Get adapter config from the server (received during handshake)
	adapterConfig := d.client.GetAdapterConfig()
	if adapterConfig == nil {
		d.client.Close()
		return fmt.Errorf("no adapter config received from server")
	}

	// Launch the debug adapter
	d.log.Info("Launching debug adapter", "args", adapterConfig.Args)
	adapter, launchErr := LaunchDebugAdapter(ctx, d.executor, adapterConfig, d.log)
	if launchErr != nil {
		d.client.Close()
		return fmt.Errorf("failed to launch debug adapter: %w", launchErr)
	}

	// Create proxy context that we can cancel independently
	proxyCtx, proxyCancel := context.WithCancel(ctx)
	defer proxyCancel()

	// Create proxy config with logger if not already set
	proxyConfig := d.proxyConfig
	if proxyConfig.Logger.GetSink() == nil {
		proxyConfig.Logger = d.log
	}

	// Create the proxy connecting upstream (IDE) to downstream (debug adapter)
	d.proxy = NewProxy(d.upstreamTransport, adapter.Transport, proxyConfig)

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
	case <-adapter.Done():
		d.log.Info("Debug adapter process exited")
	}

	// Shutdown sequence: proxy first, then adapter transport, then client
	proxyCancel()
	proxyWg.Wait()

	// Close the adapter transport (this will also help clean up the process)
	adapter.Transport.Close()

	// Wait for adapter process to fully exit
	adapterErr := adapter.Wait()

	clientErr := d.client.Close()

	// Filter and aggregate errors
	proxyErr = filterContextError(proxyErr, ctx, d.log)
	adapterErr = filterContextError(adapterErr, ctx, d.log)
	clientErr = filterContextError(clientErr, ctx, d.log)

	return errors.Join(proxyErr, adapterErr, clientErr)
}

// Proxy returns the proxy instance. Only valid after Run has started.
func (d *SessionDriver) Proxy() *Proxy {
	return d.proxy
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
			// Extract and send capabilities to the server
			d.sendCapabilitiesToServer(m)
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

// sendCapabilitiesToServer extracts capabilities from InitializeResponse and sends to the gRPC server.
func (d *SessionDriver) sendCapabilitiesToServer(resp *dap.InitializeResponse) {
	// Serialize just the body (capabilities) to JSON
	capabilitiesJSON, jsonErr := json.Marshal(resp.Body)
	if jsonErr != nil {
		d.log.Error(jsonErr, "Failed to serialize capabilities")
		return
	}

	d.log.V(1).Info("Sending capabilities to server", "size", len(capabilitiesJSON))

	if sendErr := d.client.SendCapabilities(capabilitiesJSON); sendErr != nil {
		d.log.Error(sendErr, "Failed to send capabilities to server")
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
