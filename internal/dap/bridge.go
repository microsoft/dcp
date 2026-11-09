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
		command := message.text("command")
		if command == "cancel" {
			if cancelErr := writer.remapRequestID(message, "arguments", true); cancelErr != nil {
				return cancelErr
			}
		}
		if writer.shutdown {
			if local {
				return nil
			}
			return fmt.Errorf("DAP request received after shutdown began")
		}
		if command == "disconnect" {
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

// Bridge forwards DAP while preserving extension payloads and request correlation in both directions.
// On ErrShutdownRequested it injects disconnect unless disconnect was already forwarded, allowing
// final messages to drain until adapter EOF or grace expiry. Other cancellation and client loss fail.
// It never synthesizes application exit or terminated events.
// Observe runs before forwarding, after response correlation, and must be bounded and leave messages unchanged.
// Private shutdown replies and uncorrelated responses are not observed.
func Bridge(ctx context.Context, client, adapter Transport, exited <-chan struct{}, grace time.Duration, observe func(Direction, Message) error) error {
	toAdapter, toClient := newMessageWriter(adapter), newMessageWriter(client)
	type result struct {
		client bool
		err    error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for {
			message, readErr := client.ReadMessage()
			if readErr != nil {
				results <- result{client: true, err: readErr}
				return
			}
			forward, matched, remapErr := toClient.remap(message)
			if remapErr != nil {
				results <- result{client: true, err: remapErr}
				return
			}
			if forward {
				if observe != nil && (message.text("type") != "response" || matched) {
					if observationErr := observe(ClientToAdapter, message); observationErr != nil {
						results <- result{client: true, err: observationErr}
						return
					}
				}
				if writeErr := toAdapter.send(message, false); writeErr != nil {
					results <- result{client: true, err: writeErr}
					return
				}
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			message, readErr := adapter.ReadMessage()
			if readErr != nil {
				results <- result{err: readErr}
				return
			}
			forward, matched, remapErr := toAdapter.remap(message)
			if remapErr != nil {
				results <- result{err: remapErr}
				return
			}
			if forward {
				if observe != nil && (message.text("type") != "response" || matched) {
					if observationErr := observe(AdapterToClient, message); observationErr != nil {
						results <- result{err: observationErr}
						return
					}
				}
				if writeErr := toClient.send(message, false); writeErr != nil {
					results <- result{err: writeErr}
					return
				}
			}
		}
	}()
	defer func() {
		_ = client.Close()
		_ = adapter.Close()
		workers.Wait()
	}()
	finish := func(completed result) error {
		if completed.client {
			return fmt.Errorf("debug client disconnected: %w", completed.err)
		}
		if errors.Is(completed.err, io.EOF) {
			return nil
		}
		return fmt.Errorf("debug adapter transport failed: %w", completed.err)
	}
	var drainTimer *time.Timer
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	drainExpired := make(chan struct{})
forwarding:
	for {
		select {
		case completed := <-results:
			select {
			case <-drainExpired:
				return fmt.Errorf("debug adapter output drain timed out")
			default:
			}
			return finish(completed)
		case <-drainExpired:
			return fmt.Errorf("debug adapter output drain timed out")
		case <-exited:
			exited = nil
			drainTimer = time.AfterFunc(2*time.Second, func() {
				close(drainExpired)
				_ = client.Close()
				_ = adapter.Close()
			})
		case <-ctx.Done():
			if !errors.Is(context.Cause(ctx), ErrShutdownRequested) {
				return context.Cause(ctx)
			}
			break forwarding
		}
	}
	// Closing on expiry also releases a writer blocked behind backpressure.
	graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer graceCancel()
	stopExpiry := context.AfterFunc(graceCtx, func() { _ = client.Close(); _ = adapter.Close() })
	defer stopExpiry()
	shutdownDone := make(chan struct{})
	var shutdownErr error
	go func() {
		defer close(shutdownDone)
		shutdownErr = toAdapter.send(Message{
			"seq": json.RawMessage("1"), "type": json.RawMessage(`"request"`),
			"command": json.RawMessage(`"disconnect"`), "arguments": json.RawMessage(`{}`),
		}, true)
	}()
	defer func() { _ = adapter.Close(); <-shutdownDone }()
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
		case completed := <-results:
			if graceCtx.Err() != nil {
				return fmt.Errorf("debug graceful shutdown timed out")
			}
			return finish(completed)
		case <-graceCtx.Done():
			return fmt.Errorf("debug graceful shutdown timed out")
		}
	}
}
