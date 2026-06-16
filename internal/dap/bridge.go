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
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/google/go-dap"
	"github.com/microsoft/dcp/pkg/process"
)

// BridgeConfig contains configuration for creating a DapBridge.
type BridgeConfig struct {
	// SessionID is the session identifier for this bridge.
	SessionID string

	// AdapterConfig contains the configuration for launching the debug adapter.
	AdapterConfig *DebugAdapterConfig

	// Executor is the process executor for managing debug adapter processes.
	// If nil, a new OS executor will be created for this purpose.
	Executor process.Executor

	// Logger for bridge operations.
	Logger logr.Logger

	// OutputHandler is called when output events are received from the debug adapter,
	// unless runInTerminal was used (in which case output is captured directly from the debugee
	// process). If nil, output events are only forwarded without additional processing.
	OutputHandler OutputHandler

	// StdoutWriter is where debugee process stdout (from runInTerminal) will be written.
	// If nil, stdout is discarded.
	StdoutWriter io.Writer

	// StderrWriter is where debugee process stderr (from runInTerminal) will be written.
	// If nil, stderr is discarded.
	StderrWriter io.Writer

	// StartDebuggingHandler is called when the debug adapter sends a startDebugging
	// reverse request. It should register a child session and return the child session ID
	// to include in the augmented configuration forwarded to the IDE.
	// If nil, startDebugging requests are forwarded to the IDE without interception.
	StartDebuggingHandler StartDebuggingHandler
}

// OutputHandler is called when output events are received from the debug adapter.
type OutputHandler interface {
	// HandleOutput is called for each output event.
	// category is "stdout", "stderr", "console", etc.
	// output is the actual output text.
	HandleOutput(category string, output string)
}

// StartDebuggingHandler is called when a debug adapter sends a startDebugging reverse request.
// It registers a child session and returns the child session ID.
type StartDebuggingHandler func(parentSessionID string, configuration map[string]any, request string) (childSessionID string, err error)

// DapBridge provides a bridge between an IDE's debug adapter client and a debug adapter host.
// It can either listen on a Unix domain socket for the IDE to connect (via Run),
// or accept an already-connected connection (via RunWithConnection).
type DapBridge struct {
	config   BridgeConfig
	executor process.Executor
	log      logr.Logger

	// ideTransport is the transport to the IDE
	ideTransport Transport

	// adapter is the launched debug adapter
	adapter *LaunchedAdapter

	// clientSupportsStartDebugging tracks whether the client indicated
	// supportsStartDebuggingRequest in its InitializeRequest.
	clientSupportsStartDebugging atomic.Bool

	// terminatedEventSeen tracks whether the adapter sent a TerminatedEvent
	terminatedEventSeen atomic.Bool

	// exitCode stores the exit code from the adapter's ExitedEvent (if received).
	// Written by the adapter reader goroutine, read after the bridge loop exits.
	exitCode atomic.Pointer[int32]

	// terminateCh is closed when the bridge terminates
	terminateCh chan struct{}

	// terminateOnce ensures terminateCh is closed only once
	terminateOnce sync.Once

	// adapterPipe is the FIFO message pipe for messages sent to the debug adapter.
	// It assigns monotonically increasing sequence numbers at write time and
	// maintains a seqMap of virtualSeq→originalIDESeq for response correlation.
	adapterPipe *MessagePipe

	// idePipe is the FIFO message pipe for messages sent to the IDE.
	// It assigns monotonically increasing sequence numbers at write time.
	idePipe *MessagePipe

	// fallbackIDESeqCounter is used for IDE-bound seq assignment when idePipe
	// has not yet been created (e.g., adapter launch failure before message loop).
	fallbackIDESeqCounter atomic.Int64
}

// NewDapBridge creates a new DAP bridge with the given configuration.
func NewDapBridge(config BridgeConfig) *DapBridge {
	log := config.Logger
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	executor := config.Executor
	if executor == nil {
		executor = process.NewOSExecutor(log)
	}

	return &DapBridge{
		config:      config,
		executor:    executor,
		log:         log,
		terminateCh: make(chan struct{}),
	}
}

// RunWithConnection runs the bridge with an already-connected IDE connection.
// This is the main entry point when using BridgeManager.
// The handshake must have already been performed by the caller.
//
// The bridge will:
// 1. Launch the debug adapter using the provided config
// 2. Forward DAP messages bidirectionally
// 3. Terminate when the context is cancelled or errors occur
func (b *DapBridge) RunWithConnection(ctx context.Context, ideConn net.Conn) error {
	return b.runWithConnectionAndConfig(ctx, ideConn, b.config.AdapterConfig)
}

// runWithConnectionAndConfig is the internal implementation that accepts an adapter config.
func (b *DapBridge) runWithConnectionAndConfig(ctx context.Context, ideConn net.Conn, adapterConfig *DebugAdapterConfig) error {
	defer b.terminate()

	b.log.Info("Bridge starting with pre-connected IDE", "sessionID", b.config.SessionID)

	// Create transport for IDE connection
	b.ideTransport = NewUnixTransportWithContext(ctx, ideConn)

	// Launch debug adapter
	b.log.V(1).Info("Launching debug adapter")
	launchErr := b.launchAdapterWithConfig(ctx, adapterConfig)
	if launchErr != nil {
		b.sendErrorToIDE(fmt.Sprintf("Failed to launch debug adapter: %v", launchErr))
		return fmt.Errorf("failed to launch debug adapter: %w", launchErr)
	}
	defer b.adapter.Close()

	b.log.Info("Debug adapter launched", "pid", b.adapter.Pid())

	// Start message forwarding
	b.log.V(1).Info("Bridge connected, starting message loop")
	return b.runMessageLoop(ctx)
}

// runWithConnectionAndAdapter runs the bridge by connecting to an already-running
// debug adapter at the given TCP address. This is used for child sessions created
// via startDebugging reverse requests where the adapter is shared with the parent.
func (b *DapBridge) runWithConnectionAndAdapter(ctx context.Context, ideConn net.Conn, adapterAddress string) error {
	defer b.terminate()

	b.log.Info("Child bridge starting with pre-connected IDE",
		"sessionID", b.config.SessionID,
		"adapterAddress", adapterAddress)

	// Create transport for IDE connection
	b.ideTransport = NewUnixTransportWithContext(ctx, ideConn)

	// Connect to existing adapter
	b.log.V(1).Info("Connecting to existing debug adapter", "address", adapterAddress)
	var connectErr error
	b.adapter, connectErr = ConnectToExistingAdapter(ctx, adapterAddress, b.log)
	if connectErr != nil {
		b.sendErrorToIDE(fmt.Sprintf("Failed to connect to debug adapter: %v", connectErr))
		return fmt.Errorf("failed to connect to existing adapter: %w", connectErr)
	}
	defer b.adapter.Close()

	b.log.Info("Connected to existing debug adapter for child session",
		"address", adapterAddress)

	// Start message forwarding
	b.log.V(1).Info("Child bridge connected, starting message loop")
	return b.runMessageLoop(ctx)
}

// launchAdapterWithConfig launches the debug adapter with the specified config.
func (b *DapBridge) launchAdapterWithConfig(ctx context.Context, config *DebugAdapterConfig) error {
	var launchErr error
	b.adapter, launchErr = LaunchDebugAdapter(ctx, b.executor, config, b.log)
	return launchErr
}

// runMessageLoop runs the bidirectional message forwarding loop.
func (b *DapBridge) runMessageLoop(ctx context.Context) error {
	// Create an independent context for the pipes. This must NOT be derived
	// from ctx because the ordered shutdown sequence needs the pipes to
	// remain alive after ctx is cancelled so that queued messages (including
	// shutdown events) can drain. The normal shutdown path uses CloseInput
	// on each pipe for a graceful drain; pipeCancel is a fallback safety net.
	pipeCtx, pipeCancel := context.WithCancel(context.Background())
	defer pipeCancel()

	// Create message pipes for both directions.
	b.adapterPipe = NewMessagePipe(pipeCtx, b.adapter.Transport, "adapterPipe", b.log)
	b.idePipe = NewMessagePipe(pipeCtx, b.ideTransport, "idePipe", b.log)
	// Suppress the generic "Writing message" log for the IDE-bound pipe.
	// Failure responses from the adapter are logged explicitly in forwardAdapterToIDE.
	b.idePipe.logFilter = func(*MessageEnvelope) bool { return false }

	// Track each goroutine independently so the shutdown sequence can
	// wait for specific goroutines in the correct order.
	var (
		adapterPipeResult   error
		idePipeResult       error
		adapterReaderResult error
		ideReaderResult     error
	)

	adapterPipeDone := make(chan struct{})
	idePipeDone := make(chan struct{})
	adapterReaderDone := make(chan struct{})
	ideReaderDone := make(chan struct{})

	// errCh collects the first error for the initial select trigger.
	errCh := make(chan error, 4)

	// Pipe writers
	go func() {
		adapterPipeResult = b.adapterPipe.Run(pipeCtx)
		close(adapterPipeDone)
		errCh <- adapterPipeResult
	}()
	go func() {
		idePipeResult = b.idePipe.Run(pipeCtx)
		close(idePipeDone)
		errCh <- idePipeResult
	}()

	// IDE → Adapter reader
	go func() {
		ideReaderResult = b.forwardIDEToAdapter(ctx)
		close(ideReaderDone)
		errCh <- ideReaderResult
	}()

	// Adapter → IDE reader
	go func() {
		adapterReaderResult = b.forwardAdapterToIDE(ctx)
		close(adapterReaderDone)
		errCh <- adapterReaderResult
	}()

	// Wait for first error, context cancellation, or adapter exit
	var loopErr error
	select {
	case <-ctx.Done():
		b.log.V(1).Info("Context cancelled, shutting down")
	case loopErr = <-errCh:
		if loopErr != nil && !isExpectedShutdownErr(loopErr) {
			b.log.Error(loopErr, "Message forwarding error")
		}
	case <-b.adapter.Done():
		b.log.V(1).Info("Debug adapter exited")
	}

	// === Ordered graceful shutdown ===
	//
	// The goal is to let the IDE-bound pipe (idePipe) drain any queued
	// messages (e.g., a disconnect response, terminated event) before
	// tearing down the IDE transport. The shutdown proceeds in dependency order:
	//
	// 1. adapter transport closed → adapter reader unblocked
	// 2. adapter reader done → no more external idePipe.Send() calls
	// 3. shutdown messages enqueued into idePipe (via Send)
	// 4. idePipe input closed → graceful drain → all messages written
	// 5. IDE transport closed → IDE reader unblocked
	// 6. IDE reader done → no more adapterPipe.Send()
	// 7. adapterPipe input closed → drain → done

	// Step 1: Close adapter transport to unblock the adapter→IDE reader.
	b.adapter.Transport.Close()

	// Step 2: Wait for adapter reader to finish. After this, no goroutine
	// will call idePipe.Send().
	<-adapterReaderDone

	// Step 3: Enqueue any final shutdown messages (e.g., TerminatedEvent)
	// into idePipe so they are written in-order by the pipe's writer goroutine.
	terminated := b.terminatedEventSeen.Load()
	if !terminated {
		if loopErr != nil && !isExpectedShutdownErr(loopErr) {
			b.sendErrorToIDE(fmt.Sprintf("Debug session ended unexpectedly: %v", loopErr))
		} else {
			b.sendTerminatedToIDE()
		}
	}

	// Step 4: Close idePipe input. The UnboundedChan drains all buffered
	// messages (including shutdown messages just enqueued) to its output
	// channel, and Run() writes them to the IDE transport.
	b.idePipe.CloseInput()
	<-idePipeDone

	// Step 5: Close IDE transport to unblock the IDE→adapter reader.
	b.ideTransport.Close()

	// Step 6: Wait for IDE reader to finish.
	<-ideReaderDone

	// Step 7: Close adapterPipe input and wait for drain.
	b.adapterPipe.CloseInput()
	<-adapterPipeDone

	// Collect errors from all goroutines.
	var errs []error
	for _, goroutineErr := range []error{adapterReaderResult, ideReaderResult, adapterPipeResult, idePipeResult} {
		if goroutineErr != nil && !isExpectedShutdownErr(goroutineErr) {
			errs = append(errs, goroutineErr)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// forwardIDEToAdapter reads messages from the IDE, intercepts as needed,
// and enqueues them to the adapterPipe for ordered writing.
func (b *DapBridge) forwardIDEToAdapter(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, readErr := b.ideTransport.ReadMessage()
		if readErr != nil {
			return fmt.Errorf("failed to read from IDE: %w", readErr)
		}

		env := NewMessageEnvelope(msg)
		b.logEnvelopeMessage("IDE -> Adapter: received message from IDE", env)

		// Intercept and potentially modify the message
		modifiedMsg, forward := b.interceptUpstreamMessage(msg)
		if !forward {
			b.logEnvelopeMessage("IDE -> Adapter: message not forwarded (handled locally)", env)
			continue
		}

		// Re-wrap if intercept returned a different message (e.g., modified typed message).
		if modifiedMsg != msg {
			env = NewMessageEnvelope(modifiedMsg)
		}

		b.logEnvelopeMessage("IDE -> Adapter: enqueueing message for adapter", env)
		b.adapterPipe.Send(env)
	}
}

// forwardAdapterToIDE reads messages from the debug adapter, intercepts as needed,
// remaps response seq values, and enqueues them to the idePipe for ordered writing.
func (b *DapBridge) forwardAdapterToIDE(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, readErr := b.adapter.Transport.ReadMessage()
		if readErr != nil {
			return fmt.Errorf("failed to read from adapter: %w", readErr)
		}

		env := NewMessageEnvelope(msg)

		// Intercept and potentially handle the message
		modifiedMsg, forward, asyncResponse := b.interceptDownstreamMessage(ctx, msg)

		// If there's an async response (e.g., RunInTerminalResponse), enqueue it
		// to the adapter pipe so it gets a proper sequence number.
		if asyncResponse != nil {
			asyncEnv := NewMessageEnvelope(asyncResponse)
			b.logEnvelopeMessage("Adapter -> IDE: enqueueing async response for adapter", asyncEnv)
			b.adapterPipe.Send(asyncEnv)
		}

		if !forward {
			b.logEnvelopeMessage("Adapter -> IDE: message not forwarded (handled locally)", env)
			continue
		}

		// Re-wrap if intercept returned a different message.
		if modifiedMsg != msg {
			env = NewMessageEnvelope(modifiedMsg)
		}

		// For response messages, restore the original IDE sequence number in
		// request_seq so the IDE can correlate the response with its request.
		b.adapterPipe.RemapResponseSeq(env)

		// Log only failure/cancelled responses — success responses and events are omitted
		// to avoid high-frequency noise during normal operation.
		if env.IsResponse() && env.Success != nil && !*env.Success {
			b.logEnvelopeMessage("Adapter -> IDE: forwarding failure response", env)
		}
		b.idePipe.Send(env)
	}
}

// interceptUpstreamMessage intercepts messages from the IDE to the adapter.
// Returns the (possibly modified) message and whether to forward it.
func (b *DapBridge) interceptUpstreamMessage(msg dap.Message) (dap.Message, bool) {
	switch req := msg.(type) {
	case *dap.InitializeRequest:
		// Observe whether the client supports startDebugging
		if req.Arguments.SupportsStartDebuggingRequest {
			b.clientSupportsStartDebugging.Store(true)
			b.log.V(1).Info("Client supports startDebugging reverse request")
		}

		return req, true

	default:
		return msg, true
	}
}

// interceptDownstreamMessage intercepts messages from the adapter to the IDE.
// Returns the (possibly modified) message, whether to forward it, and an optional async response.
func (b *DapBridge) interceptDownstreamMessage(ctx context.Context, msg dap.Message) (dap.Message, bool, dap.Message) {
	switch m := msg.(type) {
	case *dap.TerminatedEvent:
		b.terminatedEventSeen.Store(true)
		b.log.Info("Sending TerminatedEvent to IDE", "origin", "adapter")
		return msg, true, nil

	case *dap.ExitedEvent:
		ec := int32(m.Body.ExitCode)
		b.exitCode.Store(&ec)
		b.log.V(1).Info("Captured exit code from ExitedEvent", "exitCode", ec)
		return msg, true, nil

	case *dap.OutputEvent:
		// Capture output for logging if not using runInTerminal
		b.handleOutputEvent(m)
		return msg, true, nil

	case *dap.RunInTerminalRequest:
		// Do not handle runInTerminal locally; forward to the IDE so the adapter
		// falls back to its own process launch when the IDE does not support it.
		return msg, true, nil

	case *dap.StartDebuggingRequest:
		return b.handleStartDebuggingRequest(m)

	default:
		return msg, true, nil
	}
}

// handleOutputEvent processes output events from the debug adapter.
func (b *DapBridge) handleOutputEvent(event *dap.OutputEvent) {
	if b.config.OutputHandler != nil {
		b.config.OutputHandler.HandleOutput(event.Body.Category, event.Body.Output)
	}
}

// childSessionIDKey is the key used to inject the child session ID into
// the startDebugging configuration forwarded to the IDE.
const childSessionIDKey = "__dcp_child_session_id"

// handleStartDebuggingRequest handles the startDebugging reverse request from
// the debug adapter. If the client supports startDebugging and a handler is
// configured, the bridge registers a child session and augments the
// configuration with bridge metadata before forwarding to the IDE.
// The IDE is responsible for sending the response back to the adapter.
// Returns (modifiedMsg, forward, asyncResponse).
func (b *DapBridge) handleStartDebuggingRequest(req *dap.StartDebuggingRequest) (dap.Message, bool, dap.Message) {
	if !b.clientSupportsStartDebugging.Load() || b.config.StartDebuggingHandler == nil {
		// Client doesn't support it or no handler configured — forward unchanged
		b.log.V(1).Info("Forwarding startDebugging to IDE without interception",
			"seq", req.Seq,
			"clientSupports", b.clientSupportsStartDebugging.Load(),
			"handlerConfigured", b.config.StartDebuggingHandler != nil)
		return req, true, nil
	}

	b.log.Info("Handling startDebugging reverse request",
		"seq", req.Seq,
		"request", req.Arguments.Request)

	childSessionID, handleErr := b.config.StartDebuggingHandler(
		b.config.SessionID,
		req.Arguments.Configuration,
		req.Arguments.Request,
	)
	if handleErr != nil {
		b.log.Error(handleErr, "Failed to register child session for startDebugging",
			"requestSeq", req.Seq)
		// Forward unchanged — let the IDE deal with it without bridge metadata
		return req, true, nil
	}

	// Augment the configuration with the child session ID so the IDE
	// knows to connect through the bridge for this child session.
	if req.Arguments.Configuration == nil {
		req.Arguments.Configuration = make(map[string]any)
	}
	req.Arguments.Configuration[childSessionIDKey] = childSessionID

	b.log.Info("StartDebugging child session registered, forwarding to IDE",
		"requestSeq", req.Seq,
		"childSessionID", childSessionID)

	// Forward the augmented request to the IDE. The IDE will send the
	// response back to the adapter once the child session is established.
	return req, true, nil
}

// to the IDE. When the idePipe is available, messages are enqueued through it so that
// sequence numbering and write serialization are handled by the pipe's writer goroutine.
// When the idePipe is not yet created (e.g., adapter launch failure before the message loop),
// messages are written directly to the IDE transport with a fallback sequence counter.
func (b *DapBridge) sendErrorToIDE(message string) {
	outputEvent := newOutputEvent(0, "stderr", message+"\n")

	if b.idePipe != nil {
		b.idePipe.Send(NewMessageEnvelope(outputEvent))
		b.sendTerminatedToIDE()
		return
	}

	if b.ideTransport == nil {
		return
	}

	outputEvent.Seq = int(b.fallbackIDESeqCounter.Add(1))
	if writeErr := b.ideTransport.WriteMessage(outputEvent); writeErr != nil {
		b.log.V(1).Info("Failed to send error OutputEvent to IDE", "error", writeErr)
		return
	}

	b.sendTerminatedToIDE()
}

// sendTerminatedToIDE sends a TerminatedEvent to the IDE so it knows the debug session has ended.
// When the idePipe is available, the event is enqueued through it; otherwise it is written
// directly to the IDE transport.
func (b *DapBridge) sendTerminatedToIDE() {
	b.log.Info("Sending TerminatedEvent to IDE", "origin", "bridge")
	terminatedEvent := newTerminatedEvent(0)

	if b.idePipe != nil {
		b.idePipe.Send(NewMessageEnvelope(terminatedEvent))
		return
	}

	if b.ideTransport == nil {
		return
	}

	terminatedEvent.Seq = int(b.fallbackIDESeqCounter.Add(1))
	if writeErr := b.ideTransport.WriteMessage(terminatedEvent); writeErr != nil {
		b.log.V(1).Info("Failed to send TerminatedEvent to IDE", "error", writeErr)
	}
}

// terminate marks the bridge as terminated.
func (b *DapBridge) terminate() {
	b.terminateOnce.Do(func() {
		close(b.terminateCh)
	})
}

// ExitCode returns the exit code captured from the adapter's ExitedEvent, or nil
// if no ExitedEvent was received. Safe to call after RunWithConnection returns.
func (b *DapBridge) ExitCode() *int32 {
	return b.exitCode.Load()
}

// logEnvelopeMessage logs a DAP message envelope at V(1) level, including raw JSON.
// Additional key-value pairs can be appended via extraKeysAndValues.
func (b *DapBridge) logEnvelopeMessage(logMsg string, env *MessageEnvelope, extraKeysAndValues ...any) {
	if !b.log.V(1).Enabled() {
		return
	}
	keysAndValues := []any{"message", env.Describe()}
	if raw, ok := env.Inner.(*RawMessage); ok {
		keysAndValues = append(keysAndValues, "rawJSON", string(raw.Data))
	} else if jsonBytes, marshalErr := json.Marshal(env.Inner); marshalErr == nil {
		keysAndValues = append(keysAndValues, "rawJSON", string(jsonBytes))
	}
	keysAndValues = append(keysAndValues, extraKeysAndValues...)
	b.log.V(1).Info(logMsg, keysAndValues...)
}
