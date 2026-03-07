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
	"os/exec"
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
}

// OutputHandler is called when output events are received from the debug adapter.
type OutputHandler interface {
	// HandleOutput is called for each output event.
	// category is "stdout", "stderr", "console", etc.
	// output is the actual output text.
	HandleOutput(category string, output string)
}

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

	// runInTerminalUsed tracks whether runInTerminal was invoked
	runInTerminalUsed atomic.Bool

	// terminatedEventSeen tracks whether the adapter sent a TerminatedEvent
	terminatedEventSeen atomic.Bool

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

// launchAdapterWithConfig launches the debug adapter with the specified config.
func (b *DapBridge) launchAdapterWithConfig(ctx context.Context, config *DebugAdapterConfig) error {
	var launchErr error
	b.adapter, launchErr = LaunchDebugAdapter(ctx, b.executor, config, b.log)
	return launchErr
}

// runMessageLoop runs the bidirectional message forwarding loop.
func (b *DapBridge) runMessageLoop(ctx context.Context) error {
	// Create a cancellable context for the pipes so we can stop the writer
	// goroutines immediately during shutdown (break-glass).
	pipeCtx, pipeCancel := context.WithCancel(ctx)
	defer pipeCancel()

	// Create message pipes for both directions.
	b.adapterPipe = NewMessagePipe(pipeCtx, b.adapter.Transport, "adapterPipe", b.log)
	b.idePipe = NewMessagePipe(pipeCtx, b.ideTransport, "idePipe", b.log)

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// Pipe writers
	wg.Add(4)
	go func() {
		defer wg.Done()
		errCh <- b.adapterPipe.Run(pipeCtx)
	}()
	go func() {
		defer wg.Done()
		errCh <- b.idePipe.Run(pipeCtx)
	}()

	// IDE → Adapter reader
	go func() {
		defer wg.Done()
		errCh <- b.forwardIDEToAdapter(ctx)
	}()

	// Adapter → IDE reader
	go func() {
		defer wg.Done()
		errCh <- b.forwardAdapterToIDE(ctx)
	}()

	// Wait for first error or context cancellation
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

	// Stop the pipe writer goroutines immediately (break-glass: don't drain).
	pipeCancel()

	// Close transports to unblock any pending reads
	b.ideTransport.Close()
	b.adapter.Transport.Close()

	// Wait for all goroutines to finish
	wg.Wait()

	// After pipes are stopped, send shutdown messages directly to the IDE
	// transport. The seq counter continues from where the pipe left off.
	terminated := b.terminatedEventSeen.Load()
	if !terminated {
		if loopErr != nil && !isExpectedShutdownErr(loopErr) {
			b.sendErrorToIDE(fmt.Sprintf("Debug session ended unexpectedly: %v", loopErr))
		} else {
			b.sendTerminatedToIDE()
		}
	}

	// Collect any remaining errors (non-blocking)
	close(errCh)
	var errs []error
	for err := range errCh {
		if err != nil && !isExpectedShutdownErr(err) {
			errs = append(errs, err)
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
		b.logEnvelopeMessage("Adapter -> IDE: received message from adapter", env)

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

		b.logEnvelopeMessage("Adapter -> IDE: enqueueing message for IDE", env)
		b.idePipe.Send(env)
	}
}

// interceptUpstreamMessage intercepts messages from the IDE to the adapter.
// Returns the (possibly modified) message and whether to forward it.
func (b *DapBridge) interceptUpstreamMessage(msg dap.Message) (dap.Message, bool) {
	switch req := msg.(type) {
	case *dap.InitializeRequest:
		// Ensure supportsRunInTerminalRequest is true
		req.Arguments.SupportsRunInTerminalRequest = true
		b.log.V(1).Info("Set supportsRunInTerminalRequest=true on InitializeRequest")
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
		return msg, true, nil

	case *dap.OutputEvent:
		// Capture output for logging if not using runInTerminal
		b.handleOutputEvent(m)
		return msg, true, nil

	case *dap.RunInTerminalRequest:
		// Handle runInTerminal locally, don't forward to IDE
		response := b.handleRunInTerminalRequest(ctx, m)
		return nil, false, response

	default:
		return msg, true, nil
	}
}

// handleOutputEvent processes output events from the debug adapter.
func (b *DapBridge) handleOutputEvent(event *dap.OutputEvent) {
	// Only capture output if runInTerminal wasn't used
	// (if runInTerminal was used, we capture directly from the process)
	if !b.runInTerminalUsed.Load() && b.config.OutputHandler != nil {
		b.config.OutputHandler.HandleOutput(event.Body.Category, event.Body.Output)
	}
}

// handleRunInTerminalRequest handles the runInTerminal reverse request.
// Returns the response to send back to the debug adapter.
// The response's Seq field is set to 0 because the adapterPipe will assign
// the actual sequence number when the message is dequeued for writing.
func (b *DapBridge) handleRunInTerminalRequest(ctx context.Context, req *dap.RunInTerminalRequest) *dap.RunInTerminalResponse {
	b.log.Info("Handling RunInTerminal request",
		"seq", req.Seq,
		"kind", req.Arguments.Kind,
		"title", req.Arguments.Title,
		"cwd", req.Arguments.Cwd,
		"args", req.Arguments.Args,
		"envCount", len(req.Arguments.Env))

	// Mark that runInTerminal was used
	b.runInTerminalUsed.Store(true)

	// Build the command
	if len(req.Arguments.Args) == 0 {
		b.log.Error(fmt.Errorf("runInTerminal request has no arguments"), "RunInTerminal failed",
			"requestSeq", req.Seq)
		return &dap.RunInTerminalResponse{
			Response: dap.Response{
				ProtocolMessage: dap.ProtocolMessage{
					Type: "response",
				},
				RequestSeq: req.Seq,
				Command:    req.Command,
				Message:    "runInTerminal requires at least one argument",
			},
		}
	}

	cmd := exec.Command(req.Arguments.Args[0], req.Arguments.Args[1:]...)
	cmd.Dir = req.Arguments.Cwd
	cmd.Stdout = b.config.StdoutWriter
	cmd.Stderr = b.config.StderrWriter

	// Set environment from the request only (do not inherit current process environment)
	if len(req.Arguments.Env) > 0 {
		env := make([]string, 0, len(req.Arguments.Env))
		for k, v := range req.Arguments.Env {
			if strVal, ok := v.(string); ok {
				env = append(env, fmt.Sprintf("%s=%s", k, strVal))
			}
		}
		cmd.Env = env
	}

	handle, startErr := b.executor.StartAndForget(cmd, process.CreationFlagsNone)

	response := &dap.RunInTerminalResponse{
		Response: dap.Response{
			ProtocolMessage: dap.ProtocolMessage{
				Type: "response",
			},
			RequestSeq: req.Seq,
			Command:    req.Command,
			Success:    startErr == nil,
		},
	}

	if startErr == nil {
		response.Body.ProcessId = int(handle.Pid)
		b.log.Info("RunInTerminal succeeded",
			"requestSeq", req.Seq,
			"processId", handle.Pid)
	} else {
		response.Message = startErr.Error()
		b.log.Error(startErr, "RunInTerminal failed",
			"requestSeq", req.Seq)
	}

	return response
}

// sendErrorToIDE sends an OutputEvent with category "stderr" followed by a TerminatedEvent to the IDE.
// This is used to report errors to the IDE (e.g., adapter launch failure) before closing the connection.
// This method writes directly to the IDE transport, bypassing the idePipe. It must only be called
// after the idePipe writer goroutine has stopped. The sequence counter is shared with the pipe
// so seq values continue monotonically.
// Errors writing to the IDE transport are logged but not returned, since the bridge is shutting down anyway.
func (b *DapBridge) sendErrorToIDE(message string) {
	if b.ideTransport == nil {
		return
	}

	seq := b.nextIDESeq()
	outputEvent := newOutputEvent(seq, "stderr", message+"\n")
	if writeErr := b.ideTransport.WriteMessage(outputEvent); writeErr != nil {
		b.log.V(1).Info("Failed to send error OutputEvent to IDE", "error", writeErr)
		return
	}

	b.sendTerminatedToIDE()
}

// sendTerminatedToIDE sends a TerminatedEvent to the IDE so it knows the debug session has ended.
// This is used when the bridge terminates due to an error and the adapter has not already sent
// a TerminatedEvent. This method writes directly to the IDE transport, bypassing the idePipe.
// It must only be called after the idePipe writer goroutine has stopped.
// Errors writing to the IDE transport are logged but not returned.
func (b *DapBridge) sendTerminatedToIDE() {
	if b.ideTransport == nil {
		return
	}

	seq := b.nextIDESeq()
	terminatedEvent := newTerminatedEvent(seq)
	if writeErr := b.ideTransport.WriteMessage(terminatedEvent); writeErr != nil {
		b.log.V(1).Info("Failed to send TerminatedEvent to IDE", "error", writeErr)
	}
}

// nextIDESeq returns the next sequence number for IDE-bound messages.
// During normal operation this counter is incremented by the idePipe writer;
// during shutdown it is incremented directly by sendErrorToIDE/sendTerminatedToIDE.
func (b *DapBridge) nextIDESeq() int {
	if b.idePipe != nil {
		return int(b.idePipe.SeqCounter.Add(1))
	}
	// Fallback: idePipe not yet created (e.g., adapter launch failure before message loop).
	return int(b.fallbackIDESeqCounter.Add(1))
}

// terminate marks the bridge as terminated.
func (b *DapBridge) terminate() {
	b.terminateOnce.Do(func() {
		close(b.terminateCh)
	})
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
