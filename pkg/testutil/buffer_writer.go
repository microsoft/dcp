// Copyright (c) Microsoft Corporation. All rights reserved.

package testutil

import (
	"bytes"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	usvc_io "github.com/microsoft/dcp/pkg/io"
)

// BufferWriter is an implementation of io.WriteCloser that writes to a (dynamically expanding) buffer.
// All methods are goroutine-safe.
// Every write operation is tracked and timestamped.
// The written data with associated timestamps can be retrieved using Chunks() method.
// The BufferWriter can also have a set of "target" writers attached.
// These writers receive all data written to the BufferWriter.
// FailNextWrite() and FailNextSync() introduce errors in the next write or sync operation, respectively.

type Chunk struct {
	Offset    int
	Length    int
	Timestamp time.Time
}

type BufferWriter struct {
	data         []byte
	chunks       []Chunk
	lock         *sync.Mutex
	closed       bool
	closedCh     chan struct{}
	closeError   error
	targets      []io.Writer
	nextWriteErr error
	nextSyncErr  error
}

func NewBufferWriter() *BufferWriter {
	return &BufferWriter{
		lock:     &sync.Mutex{},
		closed:   false,
		closedCh: make(chan struct{}),
	}
}

func (bw *BufferWriter) Write(p []byte) (int, error) {
	bw.lock.Lock()
	defer bw.lock.Unlock()

	if bw.nextWriteErr != nil {
		err := bw.nextWriteErr
		bw.nextWriteErr = nil
		return 0, err
	}

	if bw.closed {
		return 0, usvc_io.ErrClosedWriter
	}
	if uint(len(bw.data)) > math.MaxUint-uint(len(p)) {
		return 0, io.ErrShortWrite
	}

	bw.chunks = append(bw.chunks, Chunk{
		Offset:    len(bw.data),
		Length:    len(p),
		Timestamp: time.Now(),
	})
	bw.data = append(bw.data, p...)

	var targetErrors error

	for _, target := range bw.targets {
		n, err := target.Write(p)
		if err != nil {
			targetErrors = errors.Join(targetErrors, err)
		} else if n != len(p) {
			targetErrors = errors.Join(targetErrors, io.ErrShortWrite)
		}
	}

	return len(p), targetErrors
}

func (bw *BufferWriter) Bytes() []byte {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	return bytes.Clone(bw.data)
}

func (bw *BufferWriter) Lines(sep []byte) [][]byte {
	bw.lock.Lock()
	defer bw.lock.Unlock()

	// The way bytes.Split() works is that it will return an empty slice as the last element
	// if the input data ends with a separator. So we need to trim it.
	retval := bytes.Split(bw.data, sep)
	if len(retval) > 1 && len(retval[len(retval)-1]) == 0 {
		retval = retval[:len(retval)-1]
	}
	return retval
}

func (bw *BufferWriter) Close() error {
	bw.lock.Lock()
	defer bw.lock.Unlock()

	if bw.closed {
		return bw.closeError
	}

	bw.closed = true

	var targetCloseErrors error
	for _, target := range bw.targets {
		if closer, isCloser := target.(io.Closer); isCloser {
			targetCloseErrors = errors.Join(targetCloseErrors, closer.Close())
		}
	}

	close(bw.closedCh)

	bw.closeError = targetCloseErrors
	return targetCloseErrors
}

func (bw *BufferWriter) Closed() <-chan struct{} {
	return bw.closedCh
}

func (bw *BufferWriter) Chunks() []Chunk {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	if bw.chunks == nil {
		return nil
	}
	return append([]Chunk{}, bw.chunks...) // make a copy
}

func (bw *BufferWriter) ChunksLen() int {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	return len(bw.chunks)
}

func (bw *BufferWriter) AddTarget(t io.Writer) error {
	bw.lock.Lock()
	defer bw.lock.Unlock()

	bw.targets = append(bw.targets, t)

	var replayErrors error
	if len(bw.chunks) > 0 {
		for _, chunk := range bw.chunks {
			n, err := t.Write(bw.data[chunk.Offset : chunk.Offset+chunk.Length])
			if err == nil && n != chunk.Length {
				err = io.ErrShortWrite
			}
			replayErrors = errors.Join(replayErrors, err)
		}
	}

	return replayErrors
}

func (bw *BufferWriter) Sync() error {
	bw.lock.Lock()
	defer bw.lock.Unlock()

	if bw.nextSyncErr != nil {
		err := bw.nextSyncErr
		bw.nextSyncErr = nil
		return err
	}

	var syncErrors error
	for _, target := range bw.targets {
		if syncer, isSyncer := target.(usvc_io.Syncer); isSyncer {
			syncErrors = errors.Join(syncErrors, syncer.Sync())
		}
	}
	return syncErrors
}

// FailNextWrite causes the next Write operation to fail with an error.
// After the failure, subsequent writes will succeed normally.
func (bw *BufferWriter) FailNextWrite(err error) {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	bw.nextWriteErr = err
}

// FailNextSync causes the next Sync operation to fail with an error.
// After the failure, subsequent syncs will succeed normally.
func (bw *BufferWriter) FailNextSync(err error) {
	bw.lock.Lock()
	defer bw.lock.Unlock()
	bw.nextSyncErr = err
}

var _ usvc_io.WriteSyncerCloser = (*BufferWriter)(nil)
