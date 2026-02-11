/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package dap

import (
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
)

// DefaultAdapterConnectionTimeout is the default timeout for connecting to the debug adapter.
const DefaultAdapterConnectionTimeout = 10 * time.Second

// DebugAdapterMode specifies how the debug adapter communicates.
type DebugAdapterMode string

const (
	// DebugAdapterModeStdio indicates the adapter uses stdin/stdout for DAP communication.
	DebugAdapterModeStdio DebugAdapterMode = "stdio"

	// DebugAdapterModeTCPCallback indicates we start a listener and adapter connects to us.
	// Pass our address to the adapter via --client-addr or similar.
	DebugAdapterModeTCPCallback DebugAdapterMode = "tcp-callback"

	// DebugAdapterModeTCPConnect indicates we specify a port, adapter listens, we connect.
	// Use {{port}} placeholder in args which is replaced with allocated port.
	DebugAdapterModeTCPConnect DebugAdapterMode = "tcp-connect"
)

// DebugAdapterConfig holds the configuration for launching a debug adapter.
// It is sent as part of the handshake request from the IDE and used internally
// to launch the adapter process.
type DebugAdapterConfig struct {
	// Args contains the command and arguments to launch the debug adapter.
	// The first element is the executable path, subsequent elements are arguments.
	// May contain "{{port}}" placeholder for TCP modes.
	Args []string `json:"args"`

	// Mode specifies how the adapter communicates.
	// Valid values: "stdio" (default), "tcp-callback", "tcp-connect".
	// An empty string is treated as "stdio".
	Mode DebugAdapterMode `json:"mode,omitempty"`

	// Env contains environment variables to set for the adapter process.
	Env []apiv1.EnvVar `json:"env,omitempty"`

	// ConnectionTimeoutSeconds is the timeout (in seconds) for connecting to the adapter in TCP modes.
	// If zero, DefaultAdapterConnectionTimeout is used.
	ConnectionTimeoutSeconds int `json:"connectionTimeoutSeconds,omitempty"`
}

// GetConnectionTimeout returns the connection timeout as a time.Duration.
// If ConnectionTimeoutSeconds is zero or negative, DefaultAdapterConnectionTimeout is returned.
func (c *DebugAdapterConfig) GetConnectionTimeout() time.Duration {
	if c.ConnectionTimeoutSeconds > 0 {
		return time.Duration(c.ConnectionTimeoutSeconds) * time.Second
	}
	return DefaultAdapterConnectionTimeout
}

// EffectiveMode returns the adapter mode, defaulting to DebugAdapterModeStdio
// if Mode is empty or unrecognized.
func (c *DebugAdapterConfig) EffectiveMode() DebugAdapterMode {
	switch c.Mode {
	case DebugAdapterModeStdio, DebugAdapterModeTCPCallback, DebugAdapterModeTCPConnect:
		return c.Mode
	default:
		return DebugAdapterModeStdio
	}
}
