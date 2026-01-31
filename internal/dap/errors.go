/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/go-logr/logr"
)

var (
	// ErrProxyClosed is returned when attempting to use a closed proxy.
	ErrProxyClosed = errors.New("proxy is closed")

	// ErrRequestTimeout is returned when a virtual request times out waiting for a response.
	ErrRequestTimeout = errors.New("request timeout")

	// ErrGRPCConnectionFailed is returned when the gRPC connection could not be established.
	ErrGRPCConnectionFailed = errors.New("gRPC connection failed")

	// ErrSessionRejected is returned when the server rejects a session (duplicate or invalid).
	ErrSessionRejected = errors.New("session rejected")

	// ErrSessionTerminated is returned when the server terminates the session.
	ErrSessionTerminated = errors.New("session terminated")

	// ErrAuthenticationFailed is returned when bearer token validation fails.
	ErrAuthenticationFailed = errors.New("authentication failed")
)

// IsConnectionError returns true if the error indicates a connection-related failure.
// This includes gRPC connection failures, session rejection, and authentication failures.
func IsConnectionError(err error) bool {
	return errors.Is(err, ErrGRPCConnectionFailed) ||
		errors.Is(err, ErrSessionRejected) ||
		errors.Is(err, ErrAuthenticationFailed)
}

// IsSessionError returns true if the error indicates a session-related failure.
// This includes session termination and session rejection.
func IsSessionError(err error) bool {
	return errors.Is(err, ErrSessionTerminated) ||
		errors.Is(err, ErrSessionRejected)
}

// IsProxyError returns true if the error indicates a proxy-related failure.
// This includes proxy closed and request timeout errors.
func IsProxyError(err error) bool {
	return errors.Is(err, ErrProxyClosed) ||
		errors.Is(err, ErrRequestTimeout)
}

// filterContextError filters out redundant context errors during shutdown.
// If the error is a context.Canceled or context.DeadlineExceeded and the
// context is already done, the error is logged at debug level and nil is returned.
// Additionally, if the error is from a process killed due to context cancellation
// (e.g., "signal: killed"), it is also filtered out.
// Otherwise, the original error is returned unchanged.
//
// This is useful when aggregating errors during shutdown to avoid including
// context cancellation errors that are expected side effects of the shutdown.
func filterContextError(err error, ctx context.Context, log logr.Logger) error {
	if err == nil {
		return nil
	}

	// Check if the context is done
	if ctx.Err() != nil {
		// Filter standard context errors
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.V(1).Info("Filtering redundant context error", "error", err)
			return nil
		}

		// Filter exec.ExitError with "signal: killed" since this is expected when
		// a process is killed due to context cancellation
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(exitErr.Error(), "signal: killed") {
			log.V(1).Info("Filtering process killed error on context cancellation", "error", err)
			return nil
		}
	}

	return err
}
