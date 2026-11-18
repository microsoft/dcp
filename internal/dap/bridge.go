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
	"sync"
	"sync/atomic"
	"time"
)

// ErrShutdownRequested requests bounded protocol shutdown instead of immediate transport teardown.
var ErrShutdownRequested = errors.New("debug resource shutdown requested")

// Transport exchanges complete DAP messages with one reader and one serialized writer.
// Close must unblock pending reads and writes.
type Transport interface {
	ReadMessage() (Message, error)
	WriteMessage(Message) error
	Close() error
}

// Direction identifies a message's originating peer.
type Direction int

const (
	ClientToAdapter Direction = iota + 1
	AdapterToClient
)

type requestIdentity struct {
	original int64
	local    bool
}

// messageWriter assigns wire sequences under the same lock that serializes complete writes.
type messageWriter struct {
	transport Transport
	writeMu   sync.Mutex
	sequence  int64
	mapMu     sync.Mutex
	requests  map[int64]requestIdentity
	shutdown  bool
}

func newMessageWriter(transport Transport) *messageWriter {
	return &messageWriter{transport: transport, requests: make(map[int64]requestIdentity)}
}

func (writer *messageWriter) send(message Message, local bool) error {
	writer.writeMu.Lock()
	defer writer.writeMu.Unlock()
	if message.text("type") == "request" {
		if writer.shutdown {
			return nil
		}
		command := message.text("command")
		if command == "cancel" {
			if cancelErr := writer.remapRequestID(message, "arguments", true); cancelErr != nil {
				return cancelErr
			}
		}
		if command == "disconnect" {
			if !local && len(message["arguments"]) != 0 {
				var arguments map[string]json.RawMessage
				if decodeErr := json.Unmarshal(message["arguments"], &arguments); decodeErr != nil {
					return fmt.Errorf("decode DAP disconnect arguments: %w", decodeErr)
				}
				if raw, present := arguments["terminateDebuggee"]; present {
					var terminate *bool
					if decodeErr := json.Unmarshal(raw, &terminate); decodeErr != nil {
						return fmt.Errorf("decode DAP disconnect terminateDebuggee: %w", decodeErr)
					}
					if terminate != nil && !*terminate {
						delete(arguments, "terminateDebuggee")
						encoded, encodeErr := json.Marshal(arguments)
						if encodeErr != nil {
							return fmt.Errorf("encode DAP disconnect arguments: %w", encodeErr)
						}
						message["arguments"] = encoded
					}
				}
			}
			writer.shutdown = true
		}
	}
	writer.sequence++
	original := message.number("seq")
	message.setNumber("seq", writer.sequence)
	if message.text("type") == "request" {
		writer.mapMu.Lock()
		if len(writer.requests) >= 4096 {
			writer.mapMu.Unlock()
			return fmt.Errorf("too many outstanding DAP requests")
		}
		writer.requests[writer.sequence] = requestIdentity{original: original, local: local}
		writer.mapMu.Unlock()
	}
	return writer.transport.WriteMessage(message)
}

func (writer *messageWriter) remapRequestID(message Message, field string, toWire bool) error {
	if len(message[field]) == 0 {
		return nil
	}
	var properties map[string]json.RawMessage
	if decodeErr := json.Unmarshal(message[field], &properties); decodeErr != nil {
		return fmt.Errorf("decode DAP %s request reference: %w", field, decodeErr)
	}
	if raw, exists := properties["requestId"]; exists {
		var reference int64
		if decodeErr := json.Unmarshal(raw, &reference); decodeErr != nil {
			return fmt.Errorf("decode DAP %s requestId: %w", field, decodeErr)
		}
		if reference <= 0 {
			return fmt.Errorf("invalid DAP %s requestId", field)
		}
		mapped := int64(0)
		writer.mapMu.Lock()
		if toWire {
			for sequence, identity := range writer.requests {
				if !identity.local && identity.original == reference {
					mapped = sequence
					break
				}
			}
		} else if identity, found := writer.requests[reference]; found && !identity.local {
			mapped = identity.original
		}
		writer.mapMu.Unlock()
		if mapped != 0 {
			properties["requestId"] = json.RawMessage(fmt.Sprint(mapped))
		} else {
			// Cancellation without a request is a no-op; progress can be unassociated.
			delete(properties, "requestId")
		}
		encoded, encodeErr := json.Marshal(properties)
		if encodeErr != nil {
			return fmt.Errorf("encode DAP %s request reference: %w", field, encodeErr)
		}
		message[field] = encoded
	}
	return nil
}

func (writer *messageWriter) remap(message Message) (bool, bool, error) {
	if message.text("type") == "event" && message.text("event") == "progressStart" {
		return true, false, writer.remapRequestID(message, "body", false)
	}
	if message.text("type") != "response" {
		return true, false, nil
	}
	wire := message.number("request_seq")
	writer.mapMu.Lock()
	identity, found := writer.requests[wire]
	delete(writer.requests, wire)
	writer.mapMu.Unlock()
	if found {
		if identity.local {
			var success bool
			if decodeErr := json.Unmarshal(message["success"], &success); decodeErr != nil || !success {
				return false, true, fmt.Errorf("adapter rejected injected disconnect: %s", message.text("message"))
			}
			return false, true, nil
		}
		message.setNumber("request_seq", identity.original)
	}
	return true, found, nil
}

type bridgeEndpoint int

const (
	bridgeClient bridgeEndpoint = iota
	bridgeAdapter
)

type bridgeResult struct {
	endpoint    bridgeEndpoint
	err         error
	progressErr error
}

type bridgeSession struct {
	client, adapter        Transport
	toAdapter, toClient    *messageWriter
	tracker                *sessionTracker
	observe                func(Direction, Message) error
	results                chan bridgeResult
	workers                sync.WaitGroup
	disconnectAcknowledged atomic.Bool
	drainTimer             *time.Timer
	drainExpired           chan struct{}
}

// Bridge forwards DAP while preserving extension payloads and request correlation in both directions.
// Client disconnect requests omit explicit terminateDebuggee=false, leaving termination to adapter defaults.
// On ErrShutdownRequested it injects disconnect unless disconnect was already forwarded, allowing
// final messages to drain until adapter EOF or grace expiry. Other cancellation and client loss fail.
// Requests received after disconnect are discarded; responses and events continue draining without synthetic replies.
// Client closure after the adapter acknowledges a client disconnect completes normally; earlier client loss fails.
// Execution exit or upstream read completion starts a bounded final-output drain.
// It never synthesizes application exit or terminated events.
// Observe runs before forwarding, after response correlation, and must be bounded and leave messages unchanged.
// Private shutdown replies and uncorrelated responses are not observed.
// ObserveState receives serialized snapshots only when connection state or DAP milestones change.
func Bridge(ctx context.Context, client, adapter Transport, exited, readDone <-chan struct{}, grace time.Duration,
	observe func(Direction, Message) error, observeState func(SessionState) error,
) (bridgeErr error) {
	session := &bridgeSession{
		client: client, adapter: adapter,
		toAdapter: newMessageWriter(adapter), toClient: newMessageWriter(client),
		tracker: &sessionTracker{publish: observeState}, observe: observe,
		results: make(chan bridgeResult, 2), drainExpired: make(chan struct{}),
	}
	defer func() {
		bridgeErr = errors.Join(bridgeErr, session.close())
	}()
	if connectedErr := session.tracker.update(func(state *SessionState) error {
		state.Phase, state.StreamsConnected = ConnectionConnected, true
		return nil
	}); connectedErr != nil {
		return connectedErr
	}
	session.workers.Add(2)
	go func() {
		defer session.workers.Done()
		session.results <- session.forwardClientToAdapter()
	}()
	go func() {
		defer session.workers.Done()
		session.results <- session.forwardAdapterToClient()
	}()

	shutdownRequested, forwardingErr := session.forwardUntilShutdown(ctx, exited, readDone)
	if forwardingErr != nil || !shutdownRequested {
		return forwardingErr
	}
	return session.disconnectAndDrain(ctx, grace)
}

func (session *bridgeSession) forwardClientToAdapter() bridgeResult {
	for {
		message, readErr := session.client.ReadMessage()
		if readErr != nil {
			return bridgeResult{endpoint: bridgeClient, err: readErr, progressErr: session.tracker.disconnect(true, readErr.Error())}
		}
		forward, matched, remapErr := session.toClient.remap(message)
		if remapErr != nil {
			return bridgeResult{endpoint: bridgeClient, err: remapErr}
		}
		if !forward {
			continue
		}
		if message.text("type") != "response" || matched {
			if progressErr := session.tracker.message(ClientToAdapter, message); progressErr != nil {
				return bridgeResult{endpoint: bridgeClient, progressErr: progressErr}
			}
			if session.observe != nil {
				if observationErr := session.observe(ClientToAdapter, message); observationErr != nil {
					return bridgeResult{endpoint: bridgeClient, err: observationErr}
				}
			}
		}
		if writeErr := session.toAdapter.send(message, false); writeErr != nil {
			return bridgeResult{endpoint: bridgeAdapter, err: writeErr, progressErr: session.tracker.disconnect(false, writeErr.Error())}
		}
	}
}

func (session *bridgeSession) forwardAdapterToClient() bridgeResult {
	for {
		message, readErr := session.adapter.ReadMessage()
		if readErr != nil {
			return bridgeResult{endpoint: bridgeAdapter, err: readErr, progressErr: session.tracker.disconnect(false, readErr.Error())}
		}
		forward, matched, remapErr := session.toAdapter.remap(message)
		if remapErr != nil {
			return bridgeResult{endpoint: bridgeAdapter, err: remapErr}
		}
		if !forward {
			continue
		}
		if message.text("type") != "response" || matched {
			if progressErr := session.tracker.message(AdapterToClient, message); progressErr != nil {
				return bridgeResult{endpoint: bridgeAdapter, progressErr: progressErr}
			}
			if session.observe != nil {
				if observationErr := session.observe(AdapterToClient, message); observationErr != nil {
					return bridgeResult{endpoint: bridgeAdapter, err: observationErr}
				}
			}
		}
		if matched && message.text("type") == "response" && message.text("command") == "disconnect" {
			var success bool
			// Publish before forwarding so immediate client closure cannot race the acknowledgement.
			session.disconnectAcknowledged.Store(json.Unmarshal(message["success"], &success) == nil && success)
		}
		if writeErr := session.toClient.send(message, false); writeErr != nil {
			return bridgeResult{endpoint: bridgeClient, err: writeErr, progressErr: session.tracker.disconnect(true, writeErr.Error())}
		}
	}
}

func (session *bridgeSession) forwardingResult(completed bridgeResult) error {
	if completed.progressErr != nil {
		return errors.Join(completed.err, completed.progressErr)
	}
	if completed.endpoint == bridgeClient {
		if session.disconnectAcknowledged.Load() {
			return nil
		}
		return fmt.Errorf("debug client disconnected: %w", completed.err)
	}
	if errors.Is(completed.err, io.EOF) {
		return nil
	}
	return fmt.Errorf("debug adapter transport failed: %w", completed.err)
}

// forwardUntilShutdown distinguishes protocol shutdown requests from completed or failed forwarding.
func (session *bridgeSession) forwardUntilShutdown(ctx context.Context, exited, readDone <-chan struct{}) (bool, error) {
	for {
		select {
		case completed := <-session.results:
			select {
			case <-session.drainExpired:
				return false, fmt.Errorf("debug adapter output drain timed out")
			default:
			}
			return false, session.forwardingResult(completed)
		case <-session.drainExpired:
			return false, fmt.Errorf("debug adapter output drain timed out")
		case <-exited:
			if terminationErr := session.tracker.update(func(state *SessionState) error {
				state.AdapterTerminating = true
				if state.Phase != ConnectionDisconnected {
					state.Phase = ConnectionTerminating
				}
				return nil
			}); terminationErr != nil {
				return false, terminationErr
			}
			exited, readDone = nil, nil
			session.startOutputDrain()
		case <-readDone:
			if disconnectedErr := session.tracker.disconnect(false, "Adapter output stream ended."); disconnectedErr != nil {
				return false, disconnectedErr
			}
			exited, readDone = nil, nil
			session.startOutputDrain()
		case <-ctx.Done():
			if terminationErr := session.tracker.update(func(state *SessionState) error {
				state.ResourceTerminating = true
				if state.Phase != ConnectionDisconnected {
					state.Phase = ConnectionTerminating
				}
				return nil
			}); terminationErr != nil {
				return false, errors.Join(context.Cause(ctx), terminationErr)
			}
			if !errors.Is(context.Cause(ctx), ErrShutdownRequested) {
				return false, context.Cause(ctx)
			}
			return true, nil
		}
	}
}

func (session *bridgeSession) startOutputDrain() {
	session.drainTimer = time.AfterFunc(2*time.Second, func() {
		close(session.drainExpired)
		_ = session.client.Close()
		_ = session.adapter.Close()
	})
}

func (session *bridgeSession) disconnectAndDrain(ctx context.Context, grace time.Duration) error {
	// Closing on expiry also releases a writer blocked behind backpressure.
	graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer graceCancel()
	stopExpiry := context.AfterFunc(graceCtx, func() { _ = session.client.Close(); _ = session.adapter.Close() })
	defer stopExpiry()
	shutdownDone := make(chan struct{})
	var shutdownErr error
	go func() {
		defer close(shutdownDone)
		shutdownErr = session.toAdapter.send(Message{
			"seq": json.RawMessage("1"), "type": json.RawMessage(`"request"`),
			"command": json.RawMessage(`"disconnect"`), "arguments": json.RawMessage(`{}`),
		}, true)
	}()
	defer func() { _ = session.adapter.Close(); <-shutdownDone }()
	shutdownResult := shutdownDone
	for {
		select {
		case <-shutdownResult:
			shutdownResult = nil
			if shutdownErr != nil {
				if graceCtx.Err() != nil {
					return fmt.Errorf("debug graceful shutdown timed out")
				}
				return fmt.Errorf("send debug disconnect: %w", shutdownErr)
			}
		case completed := <-session.results:
			if graceCtx.Err() != nil {
				return fmt.Errorf("debug graceful shutdown timed out")
			}
			return session.forwardingResult(completed)
		case <-graceCtx.Done():
			return fmt.Errorf("debug graceful shutdown timed out")
		}
	}

}

func (session *bridgeSession) close() error {
	if session.drainTimer != nil {
		session.drainTimer.Stop()
	}
	_ = session.client.Close()
	_ = session.adapter.Close()
	session.workers.Wait()
	return errors.Join(session.tracker.disconnect(true, "Stream closed by bridge."),
		session.tracker.disconnect(false, "Stream closed by bridge."))
}
