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

// projectDebugOutput buffers adapter output without blocking on client consumption.
// Overflow fails the session independently of the forwarding worker.
type projectDebugOutput struct {
	io.WriteCloser
	reader  io.ReadCloser
	failure context.Context
	fail    context.CancelCauseFunc
}

func newProjectDebugOutput(limit uint) *projectDebugOutput {
	reader, writer := usvc_io.NewBufferedPipeWithOverflowLimit(limit)
	failure, fail := context.WithCancelCause(context.Background())
	return &projectDebugOutput{reader: reader, WriteCloser: writer, failure: failure, fail: fail}
}

func (output *projectDebugOutput) Write(payload []byte) (int, error) {
	count, writeErr := output.WriteCloser.Write(payload)
	if errors.Is(writeErr, usvc_io.ErrBufferFull) {
		output.fail(writeErr)
	}
	return count, writeErr
}

func (output *projectDebugOutput) detach() error {
	return errors.Join(output.Close(), output.reader.Close())
}

// bufferProjectDebugTransport applies the same output policy to network adapter connections.
func bufferProjectDebugTransport(transport *projectDebugTransport) *projectDebugTransport {
	output := newProjectDebugOutput(projectDebugOutputLimit)
	done := make(chan struct{})
	stopping := make(chan struct{})
	go func() {
		defer close(done)
		_, copyErr := io.Copy(output, transport.Reader)
		if copyErr != nil {
			select {
			case <-stopping:
			default:
				output.fail(copyErr)
			}
		}
		_ = output.Close()
	}()
	return &projectDebugTransport{
		Reader: output.reader, Writer: transport.Writer, failure: output.failure,
		close: func() error {
			close(stopping)
			closeErr := errors.Join(transport.Close(), output.detach())
			<-done
			return closeErr
		},
	}
}
