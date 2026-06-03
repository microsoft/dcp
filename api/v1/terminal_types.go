/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// TerminalSocketMode selects how DCP establishes the HMP v1 connection over the
// terminal Unix domain socket.
//
// +kubebuilder:validation:Enum=listen;connect
type TerminalSocketMode string

const (
	// TerminalSocketModeListen means DCP listens on the socket and the client connects to it.
	TerminalSocketModeListen TerminalSocketMode = "listen"
	// TerminalSocketModeConnect means the client listens on the socket and DCP connects to it.
	TerminalSocketModeConnect TerminalSocketMode = "connect"
)

// Normalized returns the effective socket mode, treating the empty value as the
// default ("listen").
func (m TerminalSocketMode) Normalized() TerminalSocketMode {
	if m == "" {
		return TerminalSocketModeListen
	}
	return m
}

// TerminalSpec configures pseudo-terminal allocation for an Executable or Container
// and the Unix domain socket that HMP v1 clients will connect to for terminal I/O.
//
// HMP spec is here: https://github.com/mitchdenny/hex1b/blob/main/docs/muxer-protocol.md
//
// Presence of this field on an Executable or Container spec activates the terminal path:
// DCP allocates a PTY for the underlying process and listens on UDSPath.
// When a client opens an HMP v1 connection, DCP starts an HMP v1 server on the connection and bridges:
//
//   - PTY output (from the process's tty)            ->  HMP v1 Output frames
//   - HMP v1 Input frames                            ->  PTY input (process stdin)
//   - HMP v1 Resize frames                           ->  PTY resize (TIOCSWINSZ / ResizePseudoConsole)
//   - Process exit                                   ->  HMP v1 Exit frame, then close
//
// +k8s:openapi-gen=true
type TerminalSpec struct {
	// UDSPath is the Unix Domain Socket path used for the HMP v1 client connection.
	// In "listen" mode (the default) DCP listens on this path and the client connects to it.
	// In "connect" mode DCP assumes the client is already listening on this path and connects to it.
	// Required.
	UDSPath string `json:"udsPath,omitempty"`

	// SocketMode selects how DCP establishes the HMP v1 connection over UDSPath.
	// "listen" (the default) means DCP listens on the socket and the client connects to it.
	// "connect" means the client listens on the socket and DCP connects to it.
	// The terminal data flow is identical in both modes; only connection establishment differs.
	// +kubebuilder:default:=listen
	SocketMode TerminalSocketMode `json:"socketMode,omitempty"`

	// Cols is the initial width of the pseudo-terminal in character columns.
	// If zero, a sensible default (80) is used.
	// +kubebuilder:default:=80
	Cols int32 `json:"cols,omitempty"`

	// Rows is the initial height of the pseudo-terminal in character rows.
	// If zero, a sensible default (24) is used.
	// +kubebuilder:default:=24
	Rows int32 `json:"rows,omitempty"`
}

// Equal reports whether two TerminalSpec values are equal.
func (ts *TerminalSpec) Equal(other *TerminalSpec) bool {
	if ts == other {
		return true
	}
	if ts == nil || other == nil {
		return false
	}
	return ts.UDSPath == other.UDSPath &&
		ts.SocketMode.Normalized() == other.SocketMode.Normalized() &&
		ts.Cols == other.Cols &&
		ts.Rows == other.Rows
}

// Validate verifies the TerminalSpec content.
func (ts *TerminalSpec) Validate(specPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	if ts == nil {
		return errorList
	}
	if ts.UDSPath == "" {
		errorList = append(errorList, field.Invalid(specPath.Child("udsPath"), ts.UDSPath, "udsPath is required."))
	}
	switch ts.SocketMode {
	case "", TerminalSocketModeListen, TerminalSocketModeConnect:
	default:
		errorList = append(errorList, field.Invalid(specPath.Child("socketMode"), ts.SocketMode,
			"socketMode must be either \"listen\" or \"connect\"."))
	}
	if ts.Cols < 0 {
		errorList = append(errorList, field.Invalid(specPath.Child("cols"), ts.Cols, "cols must be non-negative."))
	}
	if ts.Rows < 0 {
		errorList = append(errorList, field.Invalid(specPath.Child("rows"), ts.Rows, "rows must be non-negative."))
	}
	return errorList
}

// ValidateUpdate verifies that the new TerminalSpec is a permissible update
// from the previous one. The terminal is wired up at process/container
// startup and is bound to the listener owned by the terminal host; mutating
// it after the fact would require tearing down and re-establishing the PTY
// session, which the runtime does not support today. So we forbid all
// post-creation changes (including adding or removing the terminal).
func (ts *TerminalSpec) ValidateUpdate(old *TerminalSpec, specPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	if old.Equal(ts) {
		return errorList
	}
	errorList = append(errorList, field.Forbidden(specPath, "terminal cannot be changed after the resource is created."))
	return errorList
}
