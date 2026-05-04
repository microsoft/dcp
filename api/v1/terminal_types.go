/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// TerminalSpec configures pseudo-terminal allocation for an Executable or
// Container replica and the HMP v1 producer endpoint that the Aspire terminal
// host connects to as a client.
//
// When Enabled is true, DCP allocates a PTY for the underlying process and
// listens on UDSPath (a Unix Domain Socket path on Linux/macOS, or a named pipe
// path on Windows in a follow-up). When the terminal host opens an HMP v1
// connection, DCP starts an HMP v1 server on the connection and bridges:
//
//	- PTY output (from the process's tty)            ->  HMP v1 Output frames
//	- HMP v1 Input frames                            ->  PTY input (process stdin)
//	- HMP v1 Resize frames                           ->  PTY resize (TIOCSWINSZ / ResizePseudoConsole)
//	- Process exit                                   ->  HMP v1 Exit frame, then close
//
// The HMP v1 wire format is defined by the Aspire dashboard's terminal host
// (see Hex1b's Hmp1Protocol). DCP's responsibility is limited to PTY
// allocation, the listener, and frame translation.
type TerminalSpec struct {
	// Enabled controls whether DCP allocates a PTY for the process and exposes
	// an HMP v1 producer endpoint at UDSPath.
	Enabled bool `json:"enabled,omitempty"`

	// UDSPath is the Unix Domain Socket path that DCP listens on for the
	// terminal host's HMP v1 client connection. Required when Enabled is true.
	UDSPath string `json:"udsPath,omitempty"`

	// Cols is the initial width of the pseudo-terminal in character columns.
	// If zero, a sensible default (80) is used.
	// +kubebuilder:default:=0
	Cols int32 `json:"cols,omitempty"`

	// Rows is the initial height of the pseudo-terminal in character rows.
	// If zero, a sensible default (24) is used.
	// +kubebuilder:default:=0
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
	return ts.Enabled == other.Enabled &&
		ts.UDSPath == other.UDSPath &&
		ts.Cols == other.Cols &&
		ts.Rows == other.Rows
}

// Validate verifies the TerminalSpec content.
func (ts *TerminalSpec) Validate(specPath *field.Path) field.ErrorList {
	errorList := field.ErrorList{}
	if ts == nil {
		return errorList
	}
	if ts.Enabled && ts.UDSPath == "" {
		errorList = append(errorList, field.Invalid(specPath.Child("udsPath"), ts.UDSPath, "udsPath is required when Enabled is true."))
	}
	if ts.Cols < 0 {
		errorList = append(errorList, field.Invalid(specPath.Child("cols"), ts.Cols, "cols must be non-negative."))
	}
	if ts.Rows < 0 {
		errorList = append(errorList, field.Invalid(specPath.Child("rows"), ts.Rows, "rows must be non-negative."))
	}
	return errorList
}

// DeepCopyInto copies the receiver, writing into out.
func (in *TerminalSpec) DeepCopyInto(out *TerminalSpec) {
	*out = *in
}

// DeepCopy returns a deep copy of the TerminalSpec.
func (in *TerminalSpec) DeepCopy() *TerminalSpec {
	if in == nil {
		return nil
	}
	out := new(TerminalSpec)
	in.DeepCopyInto(out)
	return out
}
