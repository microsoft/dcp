/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ctrlutil

import (
	"fmt"
	"io"
	"net/http"

	"github.com/go-logr/logr"
	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// LoggingWriteCloser is an implementation of io.WriteCloser that also logs results of all writes
// and results of the Close() call to a given log.
// If the passed inner writer implements http.Flusher interface,
// it will also flush after every write.

type loggingWriteCloser struct {
	log     logr.Logger
	inner   io.Writer
	closer  io.Closer
	flusher http.Flusher
}

func NewLoggingWriteCloser(log logr.Logger, inner io.Writer) usvc_io.WriteSyncerCloser {
	lwc := loggingWriteCloser{
		log:   log,
		inner: inner,
	}
	if closer, ok := inner.(io.Closer); ok {
		lwc.closer = closer
	}
	if flusher, ok := inner.(http.Flusher); ok {
		lwc.flusher = flusher
	}
	return &lwc
}

func (lwc *loggingWriteCloser) Write(p []byte) (int, error) {
	n, err := lwc.inner.Write(p)
	if err != nil {
		lwc.log.Error(err, "Error writing to inner writer",
			"Bytes", p,
			"Written", n,
		)
	} else {
		lwc.log.V(1).Info("Write succeeded",
			"Bytes", p,
			"Written", n,
		)
	}
	if lwc.flusher != nil {
		lwc.flusher.Flush()
	}
	return n, err
}

func (lwc *loggingWriteCloser) Close() error {
	if lwc.closer == nil {
		return nil // No-op since the inner writer is not a closer.
	}

	err := lwc.closer.Close()
	if err != nil {
		lwc.log.Error(err, "Error closing inner writer")
	} else {
		lwc.log.V(1).Info("Inner writer closed")
	}
	return err
}

func (lwc *loggingWriteCloser) Sync() error {
	if lwc.flusher != nil {
		return nil // We flush after write, so no-op here.
	}
	return fmt.Errorf("inner writer does not support Sync()")
}
