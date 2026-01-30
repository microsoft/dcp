/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/go-dap"
)

// TestClient is a DAP client for testing purposes.
// It provides helper methods for common DAP operations.
type TestClient struct {
	transport Transport
	seq       int
	seqMu     sync.Mutex

	// eventChan receives events from the server
	eventChan chan dap.Message

	// responseChans tracks pending requests waiting for responses
	responseChans map[int]chan dap.Message
	responseMu    sync.Mutex

	// ctx controls the client lifecycle
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks reader goroutine
	wg sync.WaitGroup
}

// NewTestClient creates a new DAP test client with the given transport.
func NewTestClient(transport Transport) *TestClient {
	ctx, cancel := context.WithCancel(context.Background())
	c := &TestClient{
		transport:     transport,
		seq:           0,
		eventChan:     make(chan dap.Message, 100),
		responseChans: make(map[int]chan dap.Message),
		ctx:           ctx,
		cancel:        cancel,
	}

	c.wg.Add(1)
	go c.readLoop()

	return c
}

// readLoop continuously reads messages from the transport and routes them.
func (c *TestClient) readLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		msg, readErr := c.transport.ReadMessage()
		if readErr != nil {
			if c.ctx.Err() != nil {
				return
			}
			// Log error and continue or return based on error type
			return
		}

		// Route based on message type
		switch m := msg.(type) {
		case dap.ResponseMessage:
			resp := m.GetResponse()
			c.responseMu.Lock()
			if ch, ok := c.responseChans[resp.RequestSeq]; ok {
				ch <- msg
				delete(c.responseChans, resp.RequestSeq)
			}
			c.responseMu.Unlock()

		case dap.EventMessage:
			select {
			case c.eventChan <- msg:
			default:
				// Event channel full, drop oldest
				select {
				case <-c.eventChan:
				default:
				}
				c.eventChan <- msg
			}
		}
	}
}

// nextSeq returns the next sequence number.
func (c *TestClient) nextSeq() int {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	c.seq++
	return c.seq
}

// sendRequest sends a request and waits for the response.
func (c *TestClient) sendRequest(ctx context.Context, req dap.RequestMessage) (dap.Message, error) {
	request := req.GetRequest()
	seq := c.nextSeq()
	request.Seq = seq

	// Create response channel
	respChan := make(chan dap.Message, 1)
	c.responseMu.Lock()
	c.responseChans[seq] = respChan
	c.responseMu.Unlock()

	// Send request
	if writeErr := c.transport.WriteMessage(req); writeErr != nil {
		c.responseMu.Lock()
		delete(c.responseChans, seq)
		c.responseMu.Unlock()
		return nil, fmt.Errorf("failed to send request: %w", writeErr)
	}

	// Wait for response
	select {
	case resp := <-respChan:
		return resp, nil
	case <-ctx.Done():
		c.responseMu.Lock()
		delete(c.responseChans, seq)
		c.responseMu.Unlock()
		return nil, ctx.Err()
	}
}

// Initialize sends an initialize request and returns the capabilities.
func (c *TestClient) Initialize(ctx context.Context) (*dap.InitializeResponse, error) {
	req := &dap.InitializeRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Type: "request"},
			Command:         "initialize",
		},
		Arguments: dap.InitializeRequestArguments{
			ClientID:                     "test-client",
			ClientName:                   "DAP Test Client",
			AdapterID:                    "go",
			Locale:                       "en-US",
			LinesStartAt1:                true,
			ColumnsStartAt1:              true,
			PathFormat:                   "path",
			SupportsRunInTerminalRequest: true,
		},
	}

	resp, sendErr := c.sendRequest(ctx, req)
	if sendErr != nil {
		return nil, sendErr
	}

	initResp, ok := resp.(*dap.InitializeResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected response type: %T", resp)
	}

	if !initResp.Success {
		return nil, fmt.Errorf("initialize failed: %s", initResp.Message)
	}

	return initResp, nil
}

// Launch sends a launch request to debug the given program.
func (c *TestClient) Launch(ctx context.Context, program string, stopOnEntry bool) error {
	args := map[string]interface{}{
		"mode":        "exec",
		"program":     program,
		"stopOnEntry": stopOnEntry,
	}
	argsJSON, marshalErr := json.Marshal(args)
	if marshalErr != nil {
		return fmt.Errorf("failed to marshal launch arguments: %w", marshalErr)
	}

	req := &dap.LaunchRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Type: "request"},
			Command:         "launch",
		},
		Arguments: argsJSON,
	}

	resp, sendErr := c.sendRequest(ctx, req)
	if sendErr != nil {
		return sendErr
	}

	launchResp, ok := resp.(*dap.LaunchResponse)
	if !ok {
		return fmt.Errorf("unexpected response type: %T", resp)
	}

	if !launchResp.Success {
		return fmt.Errorf("launch failed: %s", launchResp.Message)
	}

	return nil
}

// SetBreakpoints sets breakpoints in the given file at the specified lines.
func (c *TestClient) SetBreakpoints(ctx context.Context, file string, lines []int) (*dap.SetBreakpointsResponse, error) {
	breakpoints := make([]dap.SourceBreakpoint, len(lines))
	for i, line := range lines {
		breakpoints[i] = dap.SourceBreakpoint{Line: line}
	}

	req := &dap.SetBreakpointsRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Type: "request"},
			Command:         "setBreakpoints",
		},
		Arguments: dap.SetBreakpointsArguments{
			Source: dap.Source{
				Path: file,
			},
			Breakpoints: breakpoints,
		},
	}

	resp, sendErr := c.sendRequest(ctx, req)
	if sendErr != nil {
		return nil, sendErr
	}

	bpResp, ok := resp.(*dap.SetBreakpointsResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected response type: %T", resp)
	}

	if !bpResp.Success {
		return nil, fmt.Errorf("setBreakpoints failed: %s", bpResp.Message)
	}

	return bpResp, nil
}

// ConfigurationDone signals that configuration is complete.
func (c *TestClient) ConfigurationDone(ctx context.Context) error {
	req := &dap.ConfigurationDoneRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Type: "request"},
			Command:         "configurationDone",
		},
	}

	resp, sendErr := c.sendRequest(ctx, req)
	if sendErr != nil {
		return sendErr
	}

	configResp, ok := resp.(*dap.ConfigurationDoneResponse)
	if !ok {
		return fmt.Errorf("unexpected response type: %T", resp)
	}

	if !configResp.Success {
		return fmt.Errorf("configurationDone failed: %s", configResp.Message)
	}

	return nil
}

// Continue resumes execution of all threads.
func (c *TestClient) Continue(ctx context.Context, threadID int) error {
	req := &dap.ContinueRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Type: "request"},
			Command:         "continue",
		},
		Arguments: dap.ContinueArguments{
			ThreadId: threadID,
		},
	}

	resp, sendErr := c.sendRequest(ctx, req)
	if sendErr != nil {
		return sendErr
	}

	contResp, ok := resp.(*dap.ContinueResponse)
	if !ok {
		return fmt.Errorf("unexpected response type: %T", resp)
	}

	if !contResp.Success {
		return fmt.Errorf("continue failed: %s", contResp.Message)
	}

	return nil
}

// Disconnect sends a disconnect request to terminate the debug session.
func (c *TestClient) Disconnect(ctx context.Context, terminateDebuggee bool) error {
	req := &dap.DisconnectRequest{
		Request: dap.Request{
			ProtocolMessage: dap.ProtocolMessage{Type: "request"},
			Command:         "disconnect",
		},
		Arguments: &dap.DisconnectArguments{
			TerminateDebuggee: terminateDebuggee,
		},
	}

	resp, sendErr := c.sendRequest(ctx, req)
	if sendErr != nil {
		return sendErr
	}

	disconnResp, ok := resp.(*dap.DisconnectResponse)
	if !ok {
		return fmt.Errorf("unexpected response type: %T", resp)
	}

	if !disconnResp.Success {
		return fmt.Errorf("disconnect failed: %s", disconnResp.Message)
	}

	return nil
}

// WaitForEvent waits for an event of the specified type.
// Returns the event or an error if timeout expires.
func (c *TestClient) WaitForEvent(eventType string, timeout time.Duration) (dap.Message, error) {
	deadline := time.After(timeout)

	for {
		select {
		case msg := <-c.eventChan:
			if event, ok := msg.(dap.EventMessage); ok {
				if event.GetEvent().Event == eventType {
					return msg, nil
				}
			}
			// Not the event we're looking for, continue waiting

		case <-deadline:
			return nil, fmt.Errorf("timeout waiting for event %q", eventType)

		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		}
	}
}

// WaitForStoppedEvent waits for a stopped event and returns the thread ID.
func (c *TestClient) WaitForStoppedEvent(timeout time.Duration) (*dap.StoppedEvent, error) {
	msg, waitErr := c.WaitForEvent("stopped", timeout)
	if waitErr != nil {
		return nil, waitErr
	}

	stoppedEvent, ok := msg.(*dap.StoppedEvent)
	if !ok {
		return nil, fmt.Errorf("unexpected event type: %T", msg)
	}

	return stoppedEvent, nil
}

// WaitForTerminatedEvent waits for a terminated event.
func (c *TestClient) WaitForTerminatedEvent(timeout time.Duration) error {
	_, waitErr := c.WaitForEvent("terminated", timeout)
	return waitErr
}

// Close closes the client and its transport.
func (c *TestClient) Close() error {
	c.cancel()
	c.wg.Wait()
	return c.transport.Close()
}
