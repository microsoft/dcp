/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package dap provides a Debug Adapter Protocol (DAP) proxy implementation.
// The proxy sits between an IDE client and a debug adapter server, forwarding
// messages bidirectionally while providing capabilities for:
//   - Message interception and modification
//   - Virtual request injection (proxy-generated requests to the adapter)
//   - RunInTerminal request handling
//   - Event deduplication for virtual request side effects
package dap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-dap"
)

var (
	// ErrProxyClosed is returned when attempting to use a closed proxy.
	ErrProxyClosed = errors.New("proxy is closed")

	// ErrRequestTimeout is returned when a virtual request times out waiting for a response.
	ErrRequestTimeout = errors.New("request timeout")
)

// ProxyConfig contains configuration options for the DAP proxy.
type ProxyConfig struct {
	// Handler is an optional message handler for intercepting and modifying messages.
	// If nil, messages are forwarded unchanged (except for initialize requests which
	// always have supportsRunInTerminalRequest set to true).
	Handler MessageHandler

	// TerminalHandler handles runInTerminal requests from the debug adapter.
	// If nil, a default stub handler is used that returns success with zero process IDs.
	TerminalHandler TerminalHandler

	// DeduplicationWindow is the time window for event deduplication.
	// Events from the adapter matching recently emitted virtual events are suppressed.
	// If zero, DefaultDeduplicationWindow is used.
	DeduplicationWindow time.Duration

	// RequestTimeout is the default timeout for virtual requests.
	// If zero, no timeout is applied (requests wait indefinitely for responses).
	RequestTimeout time.Duration

	// Logger is the logger for the proxy. If nil, logging is disabled.
	Logger logr.Logger

	// UpstreamQueueSize is the size of the upstream message queue.
	// If zero, defaults to 100.
	UpstreamQueueSize int

	// DownstreamQueueSize is the size of the downstream message queue.
	// If zero, defaults to 100.
	DownstreamQueueSize int
}

// Proxy is a DAP proxy that sits between an IDE and a debug adapter.
type Proxy struct {
	// upstream is the transport to the IDE client
	upstream Transport

	// downstream is the transport to the debug adapter server
	downstream Transport

	// upstreamQueue holds messages to be sent to the IDE
	upstreamQueue chan dap.Message

	// downstreamQueue holds messages to be sent to the debug adapter
	downstreamQueue chan dap.Message

	// pendingRequests tracks requests awaiting responses
	pendingRequests *pendingRequestMap

	// adapterSeq generates sequence numbers for messages sent to the adapter
	adapterSeq *sequenceCounter

	// ideSeq generates sequence numbers for messages sent to the IDE
	ideSeq *sequenceCounter

	// handler is the message handler for modification/interception
	handler MessageHandler

	// terminalHandler handles runInTerminal requests
	terminalHandler TerminalHandler

	// deduplicator suppresses duplicate events from virtual requests
	deduplicator *eventDeduplicator

	// requestTimeout is the default timeout for virtual requests
	requestTimeout time.Duration

	// log is the logger for the proxy
	log logr.Logger

	// ctx is the lifecycle context for the proxy
	ctx context.Context

	// cancel cancels the lifecycle context
	cancel context.CancelFunc

	// wg tracks running goroutines for graceful shutdown
	wg sync.WaitGroup

	// startOnce ensures Start is only called once
	startOnce sync.Once

	// started indicates whether the proxy has been started
	started bool

	// mu protects started flag
	mu sync.Mutex
}

// NewProxy creates a new DAP proxy with the given transports and configuration.
func NewProxy(upstream, downstream Transport, config ProxyConfig) *Proxy {
	upstreamQueueSize := config.UpstreamQueueSize
	if upstreamQueueSize <= 0 {
		upstreamQueueSize = 100
	}

	downstreamQueueSize := config.DownstreamQueueSize
	if downstreamQueueSize <= 0 {
		downstreamQueueSize = 100
	}

	dedupWindow := config.DeduplicationWindow
	if dedupWindow == 0 {
		dedupWindow = DefaultDeduplicationWindow
	}

	handler := config.Handler
	terminalHandler := config.TerminalHandler
	if terminalHandler == nil {
		terminalHandler = defaultTerminalHandler()
	}

	log := config.Logger
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	// Compose the user handler with our required initialize request handler
	composedHandler := ComposeHandlers(initializeRequestHandler(), handler)

	return &Proxy{
		upstream:        upstream,
		downstream:      downstream,
		upstreamQueue:   make(chan dap.Message, upstreamQueueSize),
		downstreamQueue: make(chan dap.Message, downstreamQueueSize),
		pendingRequests: newPendingRequestMap(),
		adapterSeq:      newSequenceCounter(),
		ideSeq:          newSequenceCounter(),
		handler:         composedHandler,
		terminalHandler: terminalHandler,
		deduplicator:    newEventDeduplicator(dedupWindow),
		requestTimeout:  config.RequestTimeout,
		log:             log,
	}
}

// Start begins the proxy message pumps and blocks until the proxy terminates.
// Returns an error if the proxy encounters a fatal error, or nil on clean shutdown.
func (p *Proxy) Start(ctx context.Context) error {
	var startErr error
	p.startOnce.Do(func() {
		startErr = p.startInternal(ctx)
	})
	return startErr
}

func (p *Proxy) startInternal(ctx context.Context) error {
	p.mu.Lock()
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.started = true
	p.mu.Unlock()

	errChan := make(chan error, 4)

	// Start the four message pump goroutines
	p.wg.Add(4)

	// Upstream reader: IDE -> Proxy
	go func() {
		defer p.wg.Done()
		if readErr := p.upstreamReader(); readErr != nil {
			p.log.Error(readErr, "Upstream reader error")
			errChan <- fmt.Errorf("upstream reader: %w", readErr)
		}
	}()

	// Downstream reader: Adapter -> Proxy
	go func() {
		defer p.wg.Done()
		if readErr := p.downstreamReader(); readErr != nil {
			p.log.Error(readErr, "Downstream reader error")
			errChan <- fmt.Errorf("downstream reader: %w", readErr)
		}
	}()

	// Upstream writer: Proxy -> IDE
	go func() {
		defer p.wg.Done()
		if writeErr := p.upstreamWriter(); writeErr != nil {
			p.log.Error(writeErr, "Upstream writer error")
			errChan <- fmt.Errorf("upstream writer: %w", writeErr)
		}
	}()

	// Downstream writer: Proxy -> Adapter
	go func() {
		defer p.wg.Done()
		if writeErr := p.downstreamWriter(); writeErr != nil {
			p.log.Error(writeErr, "Downstream writer error")
			errChan <- fmt.Errorf("downstream writer: %w", writeErr)
		}
	}()

	// Wait for first error or context cancellation
	var result error
	select {
	case result = <-errChan:
		p.log.Info("Proxy terminating due to error", "error", result)
	case <-p.ctx.Done():
		p.log.Info("Proxy terminating due to context cancellation")
		result = p.ctx.Err()
	}

	// Trigger shutdown
	p.cancel()

	// Close transports to unblock readers
	if closeErr := p.upstream.Close(); closeErr != nil {
		p.log.Error(closeErr, "Error closing upstream transport")
	}
	if closeErr := p.downstream.Close(); closeErr != nil {
		p.log.Error(closeErr, "Error closing downstream transport")
	}

	// Close queues to unblock writers
	close(p.upstreamQueue)
	close(p.downstreamQueue)

	// Drain pending requests
	p.pendingRequests.DrainWithError()

	// Wait for all goroutines to finish
	p.wg.Wait()

	return result
}

// upstreamReader reads messages from the IDE and processes them.
func (p *Proxy) upstreamReader() error {
	for {
		select {
		case <-p.ctx.Done():
			return nil
		default:
		}

		msg, readErr := p.upstream.ReadMessage()
		if readErr != nil {
			// Check if we're shutting down
			if p.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("failed to read from IDE: %w", readErr)
		}

		p.log.V(1).Info("Received message from IDE", "type", fmt.Sprintf("%T", msg))

		// Apply handler for potential modification/interception
		modified, forward := p.handler(msg, Upstream)
		if !forward {
			p.log.V(1).Info("Message suppressed by handler")
			continue
		}
		if modified != nil {
			msg = modified
		}

		// Process based on message type
		switch m := msg.(type) {
		case dap.RequestMessage:
			p.handleIDERequestMessage(msg, m.GetRequest())
		default:
			// Forward other message types (shouldn't happen from IDE)
			p.log.Info("Unexpected message type from IDE", "type", fmt.Sprintf("%T", msg))
		}
	}
}

// handleIDERequestMessage processes a request from the IDE.
// The fullMsg is the complete typed message (e.g., *ContinueRequest), and req is the embedded Request.
func (p *Proxy) handleIDERequestMessage(fullMsg dap.Message, req *dap.Request) {
	// Assign virtual sequence number
	virtualSeq := p.adapterSeq.Next()

	// Track pending request
	p.pendingRequests.Add(virtualSeq, &pendingRequest{
		originalSeq:  req.Seq,
		virtual:      false,
		responseChan: nil,
		request:      fullMsg,
	})

	// Update sequence number and forward the full message
	originalSeq := req.Seq
	req.Seq = virtualSeq

	p.log.V(1).Info("Forwarding request to adapter",
		"command", req.Command,
		"originalSeq", originalSeq,
		"virtualSeq", virtualSeq)

	select {
	case p.downstreamQueue <- fullMsg:
	case <-p.ctx.Done():
	}
}

// downstreamReader reads messages from the debug adapter and processes them.
func (p *Proxy) downstreamReader() error {
	for {
		select {
		case <-p.ctx.Done():
			return nil
		default:
		}

		msg, readErr := p.downstream.ReadMessage()
		if readErr != nil {
			// Check if we're shutting down
			if p.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("failed to read from adapter: %w", readErr)
		}

		p.log.V(1).Info("Received message from adapter", "type", fmt.Sprintf("%T", msg))

		// Apply handler for potential modification/interception
		modified, forward := p.handler(msg, Downstream)
		if !forward {
			p.log.V(1).Info("Message suppressed by handler")
			continue
		}
		if modified != nil {
			msg = modified
		}

		// Process based on message type
		switch m := msg.(type) {
		case dap.ResponseMessage:
			p.handleAdapterResponseMessage(msg, m.GetResponse())
		case dap.EventMessage:
			p.handleAdapterEventMessage(msg, m.GetEvent())
		case *dap.RunInTerminalRequest:
			p.handleRunInTerminalRequest(m)
		case dap.RequestMessage:
			// Other reverse requests - forward to IDE
			p.forwardToIDE(msg)
		default:
			p.log.Info("Unexpected message type from adapter", "type", fmt.Sprintf("%T", msg))
		}
	}
}

// handleAdapterResponseMessage processes a response from the debug adapter.
// The fullMsg is the complete typed message, and resp is the embedded Response.
func (p *Proxy) handleAdapterResponseMessage(fullMsg dap.Message, resp *dap.Response) {
	// Look up the pending request
	pending := p.pendingRequests.Get(resp.RequestSeq)
	if pending == nil {
		p.log.Info("Received response for unknown request", "requestSeq", resp.RequestSeq)
		return
	}

	if pending.virtual {
		// Virtual request - deliver the full message to channel
		if pending.responseChan != nil {
			select {
			case pending.responseChan <- fullMsg:
			default:
				p.log.Info("Virtual response channel full, dropping response")
			}
			close(pending.responseChan)
		}
		return
	}

	// Real request from IDE - restore original sequence number and forward
	resp.RequestSeq = pending.originalSeq
	p.forwardToIDE(fullMsg)
}

// handleAdapterEventMessage processes an event from the debug adapter.
// The fullMsg is the complete typed message, and event is the embedded Event.
func (p *Proxy) handleAdapterEventMessage(fullMsg dap.Message, event *dap.Event) {
	// Check for deduplication
	if p.deduplicator.ShouldSuppress(fullMsg) {
		p.log.V(1).Info("Suppressing duplicate event", "event", event.Event)
		return
	}

	p.forwardToIDE(fullMsg)
}

// handleRunInTerminalRequest handles a runInTerminal reverse request from the adapter.
func (p *Proxy) handleRunInTerminalRequest(req *dap.RunInTerminalRequest) {
	p.log.Info("Intercepting runInTerminal request",
		"kind", req.Arguments.Kind,
		"title", req.Arguments.Title,
		"cwd", req.Arguments.Cwd)

	// Invoke the terminal handler
	response := p.terminalHandler(req)

	// Set the response sequence number
	response.Seq = p.adapterSeq.Next()
	response.RequestSeq = req.Seq

	// Send response back to adapter
	select {
	case p.downstreamQueue <- response:
	case <-p.ctx.Done():
	}
}

// forwardToIDE sends a message to the IDE.
func (p *Proxy) forwardToIDE(msg dap.Message) {
	select {
	case p.upstreamQueue <- msg:
	case <-p.ctx.Done():
	}
}

// upstreamWriter writes messages from the queue to the IDE.
func (p *Proxy) upstreamWriter() error {
	for {
		select {
		case msg, ok := <-p.upstreamQueue:
			if !ok {
				return nil
			}

			if writeErr := p.upstream.WriteMessage(msg); writeErr != nil {
				if p.ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("failed to write to IDE: %w", writeErr)
			}

			p.log.V(1).Info("Sent message to IDE", "type", fmt.Sprintf("%T", msg))

		case <-p.ctx.Done():
			return nil
		}
	}
}

// downstreamWriter writes messages from the queue to the debug adapter.
func (p *Proxy) downstreamWriter() error {
	for {
		select {
		case msg, ok := <-p.downstreamQueue:
			if !ok {
				return nil
			}

			if writeErr := p.downstream.WriteMessage(msg); writeErr != nil {
				if p.ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("failed to write to adapter: %w", writeErr)
			}

			p.log.V(1).Info("Sent message to adapter", "type", fmt.Sprintf("%T", msg))

		case <-p.ctx.Done():
			return nil
		}
	}
}

// SendRequest sends a virtual request to the debug adapter and waits for the response.
// This method blocks until a response is received or the context is cancelled.
func (p *Proxy) SendRequest(ctx context.Context, request dap.Message) (dap.Message, error) {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return nil, ErrProxyClosed
	}
	p.mu.Unlock()

	// Check proxy context
	if p.ctx.Err() != nil {
		return nil, ErrProxyClosed
	}

	// Create response channel
	responseChan := make(chan dap.Message, 1)

	// Get the request and assign sequence number
	var req *dap.Request
	switch r := request.(type) {
	case *dap.Request:
		req = r
	case dap.RequestMessage:
		req = r.GetRequest()
	default:
		return nil, fmt.Errorf("expected request message, got %T", request)
	}

	virtualSeq := p.adapterSeq.Next()
	originalSeq := req.Seq
	req.Seq = virtualSeq

	// Track as pending virtual request
	p.pendingRequests.Add(virtualSeq, &pendingRequest{
		originalSeq:  originalSeq,
		virtual:      true,
		responseChan: responseChan,
		request:      request,
	})

	p.log.V(1).Info("Sending virtual request",
		"command", req.Command,
		"virtualSeq", virtualSeq)

	// Send to adapter
	select {
	case p.downstreamQueue <- request:
	case <-ctx.Done():
		// Clean up pending request
		p.pendingRequests.Get(virtualSeq)
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, ErrProxyClosed
	}

	// Apply timeout if configured
	waitCtx := ctx
	if p.requestTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, p.requestTimeout)
		defer cancel()
	}

	// Wait for response
	select {
	case response, ok := <-responseChan:
		if !ok {
			return nil, ErrProxyClosed
		}
		return response, nil
	case <-waitCtx.Done():
		// Clean up pending request if still there
		p.pendingRequests.Get(virtualSeq)
		if waitCtx.Err() == context.DeadlineExceeded {
			return nil, ErrRequestTimeout
		}
		return nil, waitCtx.Err()
	case <-p.ctx.Done():
		return nil, ErrProxyClosed
	}
}

// SendRequestAsync sends a virtual request to the debug adapter asynchronously.
// The response will be delivered to the provided channel. The channel is closed
// after the response is delivered or if an error occurs.
func (p *Proxy) SendRequestAsync(request dap.Message, responseChan chan<- dap.Message) error {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return ErrProxyClosed
	}
	p.mu.Unlock()

	if p.ctx.Err() != nil {
		return ErrProxyClosed
	}

	// Get the request and assign sequence number
	var req *dap.Request
	switch r := request.(type) {
	case *dap.Request:
		req = r
	case dap.RequestMessage:
		req = r.GetRequest()
	default:
		return fmt.Errorf("expected request message, got %T", request)
	}

	virtualSeq := p.adapterSeq.Next()
	originalSeq := req.Seq
	req.Seq = virtualSeq

	// Create internal response channel that wraps the user's channel
	internalChan := make(chan dap.Message, 1)

	// Track as pending virtual request
	p.pendingRequests.Add(virtualSeq, &pendingRequest{
		originalSeq:  originalSeq,
		virtual:      true,
		responseChan: internalChan,
		request:      request,
	})

	// Start goroutine to forward response
	go func() {
		defer close(responseChan)
		select {
		case response, ok := <-internalChan:
			if ok {
				select {
				case responseChan <- response:
				default:
				}
			}
		case <-p.ctx.Done():
		}
	}()

	p.log.V(1).Info("Sending async virtual request",
		"command", req.Command,
		"virtualSeq", virtualSeq)

	// Send to adapter
	select {
	case p.downstreamQueue <- request:
		return nil
	case <-p.ctx.Done():
		// Clean up pending request
		p.pendingRequests.Get(virtualSeq)
		return ErrProxyClosed
	}
}

// EmitEvent sends a proxy-generated event to the IDE.
// The event is also recorded for deduplication so that matching events
// from the adapter will be suppressed.
func (p *Proxy) EmitEvent(event dap.Message) error {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return ErrProxyClosed
	}
	p.mu.Unlock()

	if p.ctx.Err() != nil {
		return ErrProxyClosed
	}

	// Record for deduplication
	p.deduplicator.RecordVirtualEvent(event)

	// Send to IDE
	select {
	case p.upstreamQueue <- event:
		return nil
	case <-p.ctx.Done():
		return ErrProxyClosed
	}
}

// Stop gracefully stops the proxy.
func (p *Proxy) Stop() {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()
}
