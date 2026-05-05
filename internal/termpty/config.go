/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

// SessionConfig captures the parameters the HMP v1 listener needs to accept
// terminal viewer connections. It mirrors the relevant subset of
// apiv1.TerminalSpec but lives in this package so it can be constructed
// from any caller (executable runner, container reconciler, ...).
type SessionConfig struct {
	// UDSPath is the Unix domain socket path the listener binds to.
	UDSPath string

	// Cols/Rows are the initial terminal dimensions the HMP v1 server
	// reports to a connecting viewer if it doesn't supply its own. If
	// either is <= 0 the defaults from normalizeDimensions are used.
	Cols int
	Rows int
}

// normalizeDimensions returns the effective dimensions to use for an
// allocated PTY or for HMP1's initial Hello frame, applying the
// 80x24 fallback when either input is non-positive.
func normalizeDimensions(cols, rows int) (int, int) {
	c, r := cols, rows
	if c <= 0 {
		c = 80
	}
	if r <= 0 {
		r = 24
	}
	return c, r
}
