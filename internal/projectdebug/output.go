/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package projectdebug

import (
	"context"
	"errors"
	"io"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// Allow eight maximum-sized DAP messages of unread output before failing a stalled session.
const projectDebugOutputLimit = 64 * 1024 * 1024

// bufferProjectDebugTransport consumes stdio and network output with one bounded session policy.
// Upstream completion starts final-output draining independently of execution lifetime.
func bufferProjectDebugTransport(transport *projectDebugTransport) *projectDebugTransport {
	reader, writer := usvc_io.NewBufferedPipeWithOverflowLimit(projectDebugOutputLimit)
	failure, fail := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	stopping := make(chan struct{})
	go func() {
		defer close(done)
		_, copyErr := io.Copy(writer, transport.Reader)
		if copyErr != nil {
			select {
			case <-stopping:
			default:
				fail(copyErr)
			}
		}
		_ = writer.Close()
	}()
	return &projectDebugTransport{
		Reader: reader, Writer: transport.Writer, failure: failure, readDone: done,
		close: func() error {
			close(stopping)
			closeErr := errors.Join(transport.Close(), writer.Close(), reader.Close())
			<-done
			return closeErr
		},
	}
}
