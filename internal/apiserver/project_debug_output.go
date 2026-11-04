/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package apiserver

import (
	"errors"
	"io"
	"sync/atomic"
)

// projectDebugOutput keeps the executor's stdout copy alive after its viewer departs.
// Detaching releases a blocked write and discards subsequent bytes until process output settles.
type projectDebugOutput struct {
	reader   *io.PipeReader
	writer   *io.PipeWriter
	detached atomic.Bool
}

func newProjectDebugOutput() *projectDebugOutput {
	reader, writer := io.Pipe()
	return &projectDebugOutput{reader: reader, writer: writer}
}

func (output *projectDebugOutput) Write(payload []byte) (int, error) {
	if output.detached.Load() {
		return len(payload), nil
	}
	written, writeErr := output.writer.Write(payload)
	if errors.Is(writeErr, io.ErrClosedPipe) && output.detached.Load() {
		return len(payload), nil
	}
	return written, writeErr
}

func (output *projectDebugOutput) detach() error {
	output.detached.Store(true)
	return output.reader.Close()
}

func (output *projectDebugOutput) Close() error {
	return output.writer.Close()
}
