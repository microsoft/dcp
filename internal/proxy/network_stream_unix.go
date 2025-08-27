//go:build !windows

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
		errors.Is(err, syscall.ECONNRESET)
}
