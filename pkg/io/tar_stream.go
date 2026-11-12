/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"context"
	"errors"
	"io"
	"sync"
)

type directoryTarStream struct {
	*io.PipeReader
	cancel      context.CancelFunc
	done        chan struct{}
	once        sync.Once
	producerErr error
	closeErr    error
}

// OpenDirectoryTar streams directory contents beneath destination in a tar archive without staging a file.
// The caller closes the reader to release the producer, including on early copy failure.
func OpenDirectoryTar(ctx context.Context, source, destination string, uid, gid int32) io.ReadCloser {
	lifetime, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	stopCancellation := context.AfterFunc(lifetime, func() { _ = writer.CloseWithError(context.Cause(lifetime)) })
	stream := &directoryTarStream{PipeReader: reader, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(stream.done)
		defer stopCancellation()
		tarWriter := NewTarWriterTo(writer)
		copyErr := tarWriter.CopyDirectory(lifetime, source, destination, uid, gid)
		finalizeErr := tarWriter.Close()
		stream.producerErr = errors.Join(copyErr, finalizeErr)
		_ = writer.CloseWithError(stream.producerErr)
	}()
	return stream
}

func (stream *directoryTarStream) Close() error {
	stream.once.Do(func() {
		stream.cancel()
		readerErr := stream.PipeReader.Close()
		<-stream.done
		stream.closeErr = errors.Join(readerErr, stream.producerErr)
	})
	return stream.closeErr
}
