/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"encoding/json"
	"fmt"
	"sync"
)

type ConnectionPhase int

const (
	ConnectionConnected ConnectionPhase = iota + 1
	ConnectionTerminating
	ConnectionDisconnected
)

// SessionState records bridge connection state and latched protocol observations.
// ObserveState callbacks receive immutable snapshots; correlation remains internal to the message writers.
type SessionState struct {
	Phase                    ConnectionPhase
	StreamsConnected         bool
	InitializeRequested      bool
	LaunchRequested          bool
	AttachRequested          bool
	AdapterInitialized       bool
	ProcessReported          bool
	ClientTerminating        bool
	AdapterTerminating       bool
	ResourceTerminating      bool
	DebuggeeExited           bool
	DebuggeeExitCode         *int32
	ClientDisconnected       bool
	AdapterDisconnected      bool
	ClientDisconnectMessage  string
	AdapterDisconnectMessage string
	Message                  string
}

type sessionTracker struct {
	mu      sync.Mutex
	state   SessionState
	publish func(SessionState) error
}

func (tracker *sessionTracker) update(change func(*SessionState) error) error {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	previous := tracker.state
	if updateErr := change(&tracker.state); updateErr != nil {
		return updateErr
	}
	if tracker.state == previous || tracker.publish == nil {
		return nil
	}
	snapshot := tracker.state
	if snapshot.DebuggeeExitCode != nil {
		code := *snapshot.DebuggeeExitCode
		snapshot.DebuggeeExitCode = &code
	}
	return tracker.publish(snapshot)
}

func (tracker *sessionTracker) message(direction Direction, message Message) error {
	return tracker.update(func(state *SessionState) error {
		kind, command := message.text("type"), message.text("command")
		if kind == "request" {
			if direction == ClientToAdapter {
				switch command {
				case "initialize":
					state.InitializeRequested = true
					state.Message = ""
				case "launch":
					state.LaunchRequested = true
					state.Message = ""
				case "attach":
					state.AttachRequested = true
					state.Message = ""
				}
			}
			if command == "terminate" || command == "disconnect" {
				if direction == ClientToAdapter {
					state.ClientTerminating = true
				} else {
					state.AdapterTerminating = true
				}
				if state.Phase != ConnectionDisconnected {
					state.Phase = ConnectionTerminating
				}
			}
		}
		if direction != AdapterToClient {
			return nil
		}
		if kind == "response" && (command == "initialize" || command == "launch" || command == "attach") {
			var success bool
			if decodeErr := json.Unmarshal(message["success"], &success); decodeErr != nil {
				return fmt.Errorf("decode DAP response success: %w", decodeErr)
			}
			if !success {
				var diagnostic string
				if raw, present := message["message"]; present {
					if decodeErr := json.Unmarshal(raw, &diagnostic); decodeErr != nil {
						return fmt.Errorf("decode DAP response message: %w", decodeErr)
					}
				}
				state.Message = fmt.Sprintf("Adapter rejected %s: %s", command, diagnostic)
			}
		}
		if kind == "event" {
			switch message.text("event") {
			case "initialized":
				state.AdapterInitialized = true
			case "process":
				state.ProcessReported = true
			case "terminated":
				state.AdapterTerminating = true
				if state.Phase != ConnectionDisconnected {
					state.Phase = ConnectionTerminating
				}
			case "exited":
				var body struct {
					ExitCode *int32 `json:"exitCode"`
				}
				if decodeErr := json.Unmarshal(message["body"], &body); decodeErr != nil || body.ExitCode == nil {
					return fmt.Errorf("invalid DAP exited event: exitCode is required and must fit int32")
				}
				if !state.DebuggeeExited {
					state.DebuggeeExited, state.DebuggeeExitCode = true, body.ExitCode
				}
			}
		}
		return nil
	})
}

func (tracker *sessionTracker) disconnect(client bool, cause string) error {
	return tracker.update(func(state *SessionState) error {
		state.Phase = ConnectionDisconnected
		if client && !state.ClientDisconnected {
			state.ClientDisconnected, state.ClientDisconnectMessage = true, cause
		} else if !client && !state.AdapterDisconnected {
			state.AdapterDisconnected, state.AdapterDisconnectMessage = true, cause
		}
		return nil
	})
}
