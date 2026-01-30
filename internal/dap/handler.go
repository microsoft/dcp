/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"github.com/google/go-dap"
)

// MessageHandler is a function that can inspect and modify DAP messages as they flow
// through the proxy. It receives the message and its flow direction, and returns:
//   - modified: the (possibly modified) message to forward
//   - forward: whether to forward the message (false to suppress)
//
// If the handler returns nil for modified but true for forward, the original message
// is forwarded unchanged.
type MessageHandler func(msg dap.Message, direction Direction) (modified dap.Message, forward bool)

// TerminalHandler is a function that handles runInTerminal requests from the debug adapter.
// It receives the request and should return a response indicating success or failure.
// The processId and shellProcessId in the response indicate the launched process IDs.
type TerminalHandler func(req *dap.RunInTerminalRequest) *dap.RunInTerminalResponse

// ComposeHandlers combines multiple message handlers into a single handler.
// Handlers are called in order; if any handler returns forward=false, the chain stops.
// The modified message from each handler is passed to the next handler.
func ComposeHandlers(handlers ...MessageHandler) MessageHandler {
	return func(msg dap.Message, direction Direction) (dap.Message, bool) {
		current := msg
		for _, h := range handlers {
			if h == nil {
				continue
			}

			modified, forward := h(current, direction)
			if !forward {
				return nil, false
			}

			if modified != nil {
				current = modified
			}
		}

		return current, true
	}
}

// initializeRequestHandler returns a handler that forces supportsRunInTerminalRequest
// to true on InitializeRequest messages. This allows the proxy to intercept terminal
// requests from the debug adapter.
func initializeRequestHandler() MessageHandler {
	return func(msg dap.Message, direction Direction) (dap.Message, bool) {
		// Only modify upstream (IDE -> adapter) initialize requests
		if direction != Upstream {
			return msg, true
		}

		initReq, ok := msg.(*dap.InitializeRequest)
		if !ok {
			return msg, true
		}

		// Force support for runInTerminal so we can intercept it
		initReq.Arguments.SupportsRunInTerminalRequest = true
		return initReq, true
	}
}

// defaultTerminalHandler returns a stub terminal handler that returns success
// with zero process IDs. This is a placeholder for future implementation.
func defaultTerminalHandler() TerminalHandler {
	return func(req *dap.RunInTerminalRequest) *dap.RunInTerminalResponse {
		response := &dap.RunInTerminalResponse{
			Response: dap.Response{
				ProtocolMessage: dap.ProtocolMessage{
					Seq:  0, // Will be set by the proxy
					Type: "response",
				},
				Command:    "runInTerminal",
				RequestSeq: req.Seq,
				Success:    true,
			},
			Body: dap.RunInTerminalResponseBody{
				ProcessId:      0,
				ShellProcessId: 0,
			},
		}

		return response
	}
}
