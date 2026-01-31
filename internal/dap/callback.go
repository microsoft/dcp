/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"github.com/google/go-dap"
)

// AsyncResponse represents an asynchronous response from a callback.
// It contains either a response message or an error.
type AsyncResponse struct {
	// Response is the DAP message to send as a response.
	// This should be nil if Err is set.
	Response dap.Message

	// Err is set if the asynchronous operation failed.
	// When set, an error response will be sent to the originator.
	Err error
}

// CallbackResult represents the result of a message callback.
// It determines how the proxy should handle the message.
type CallbackResult struct {
	// Modified is the modified message to forward.
	// If nil and Forward is true, the original message is forwarded unchanged.
	Modified dap.Message

	// Forward indicates whether to forward the message to the other side.
	// If false, the message is suppressed.
	Forward bool

	// ResponseChan provides an asynchronous response when Forward is false.
	// If non-nil, the proxy will wait for a response on this channel and
	// send it back to the message originator. The channel should be closed
	// after sending the response.
	ResponseChan <-chan AsyncResponse

	// Err indicates an immediate fatal error during callback processing.
	// When set, the proxy will terminate with this error.
	// This is different from AsyncResponse.Err which is a non-fatal operation error.
	Err error
}

// MessageCallback is a function that processes DAP messages as they flow through the proxy.
// It receives the message and returns a CallbackResult that determines how the message
// should be handled.
//
// Callbacks run on the reader goroutines. If a callback blocks (e.g., waiting for a
// response channel), it will block the corresponding reader. This is intentional for
// cases like RunInTerminal where no other messages should be processed until the
// response is received.
type MessageCallback func(msg dap.Message) CallbackResult

// ForwardUnchanged returns a CallbackResult that forwards the message unchanged.
func ForwardUnchanged() CallbackResult {
	return CallbackResult{
		Forward: true,
	}
}

// ForwardModified returns a CallbackResult that forwards a modified message.
func ForwardModified(msg dap.Message) CallbackResult {
	return CallbackResult{
		Modified: msg,
		Forward:  true,
	}
}

// Suppress returns a CallbackResult that suppresses the message without sending a response.
func Suppress() CallbackResult {
	return CallbackResult{
		Forward: false,
	}
}

// SuppressWithAsyncResponse returns a CallbackResult that suppresses the message
// and provides an asynchronous response channel. The proxy will wait for a response
// on the channel and send it back to the message originator.
func SuppressWithAsyncResponse(ch <-chan AsyncResponse) CallbackResult {
	return CallbackResult{
		Forward:      false,
		ResponseChan: ch,
	}
}

// CallbackError returns a CallbackResult that indicates a fatal error.
// The proxy will terminate with this error.
func CallbackError(err error) CallbackResult {
	return CallbackResult{
		Err: err,
	}
}
