/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

//go:build windows

// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// Used to suppress reporting of errors that are expected when the connection is closed.
func isExpectedConnCloseErr(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.WSAECONNRESET)
}
