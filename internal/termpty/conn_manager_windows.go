//go:build windows

/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package termpty

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// isExpectedClientDisconnect reports whether err reflects the HMP client closing
// or dropping the connection (peer hang-up, connection reset, EOF, or a closed
// connection). These are normal lifecycle events — including a viewer that
// disconnects mid-handshake, before the server finishes writing the
// Hello/StateSync frames — and are not server failures worth logging at Error level.
func isExpectedClientDisconnect(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.WSAECONNRESET)
}
