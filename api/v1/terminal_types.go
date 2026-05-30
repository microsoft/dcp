/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// TerminalSpec configures pseudo-terminal allocation for an Executable or Container
// and the Unix domain socket that DCP DIALS to deliver HMP v1 terminal I/O.
//
// HMP spec is here: https://github.com/mitchdenny/hex1b/blob/main/docs/muxer-protocol.md
//
// Presence of this field on an Executable or Container spec activates the terminal path:
// DCP allocates a PTY for the underlying process and dials the HMP v1 listener at UDSPath.
// The peer (the Aspire terminal host) owns the listener and the socket file.
// Once the HMP v1 connection is established DCP starts an HMP v1 server on the connection
// and bridges:
//
//   - PTY output (from the process's tty)            ->  HMP v1 Output frames
//   - HMP v1 Input frames                            ->  PTY input (process stdin)
//   - HMP v1 Resize frames                           ->  PTY resize (TIOCSWINSZ / ResizePseudoConsole)
//   - Process exit                                   ->  HMP v1 Exit frame, then close
//
// +k8s:openapi-gen=true
type TerminalSpec struct {
	// UDSPath is the Unix Domain Socket path that DCP DIALS to deliver HMP v1
	// terminal I/O. The peer (the Aspire terminal host) is the listener and
	// owns the socket file lifecycle.
	// Required.
	UDSPath string `json:"udsPath,omitempty"`

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
