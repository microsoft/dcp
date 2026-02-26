/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build !windows

package dap

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// isExpectedCloseErr returns true if the error is expected when a network
// connection or pipe is closed. This is used to suppress error-level logging
// for errors that occur as a normal consequence of shutting down transports.
func isExpectedCloseErr(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET)
}
