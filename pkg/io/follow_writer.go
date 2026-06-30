/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package io

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultBufferSize = 1024
	readRetryInterval = 200 * time.Millisecond
)

// FollowWriterOption is a functional option for the FollowWriter.
type FollowWriterOption func(*FollowWriter)

// WithNoDataStopRetries sets the number of extra read attempts the FollowWriter will make
// after StopFollow() is called, but only if it has never read any data.
// If the FollowWriter has already seen data when StopFollow() is called, it stops immediately after zero-byte read
// or EOF is encountered.
func WithNoDataStopRetries(n uint) FollowWriterOption {
	return func(fw *FollowWriter) {
		fw.noDataStopRetries = n
	}
}

// WithCloseSourceOnCancel makes Cancel synchronously close source if it implements io.Closer.
// Once enabled, the FollowWriter owns source closure and closes it before Done on any exit.
func WithCloseSourceOnCancel() FollowWriterOption {
	return func(fw *FollowWriter) {
		fw.closeSourceOnCancel = true
	}
}

type FollowWriter struct {
	err                 atomic.Value
	follow              atomic.Bool
	cancel              context.CancelFunc
	closeSourceOnCancel bool
	closeSourceOnce     func()
	doneChan            chan struct{}
	noDataStopRetries   uint
}

// Creates a FollowWriter that reads content from the reader source and writes it to the writer destination.
// Keeps trying to read new content even after EOF until StopFollow() is called, after which the next EOF
// received will cause the reader and writer to stop.
// Source ownership stays with the caller unless WithCloseSourceOnCancel is used.
//
// Use WithNoDataStopRetries option to specify extra read attempts after StopFollow() is called
// when no data has been seen yet. This is useful when the data source might not be ready immediately.
func NewFollowWriter(ctx context.Context, source io.Reader, dest io.Writer, opts ...FollowWriterOption) *FollowWriter {
	followCtx, cancel := context.WithCancel(ctx)
	closeSourceOnce := sync.OnceFunc(func() {})
	if sourceCloser, isCloser := source.(io.Closer); isCloser {
		closeSourceOnce = sync.OnceFunc(func() {
			_ = sourceCloser.Close()
		})
	}
	fw := &FollowWriter{
		err:             atomic.Value{},
		follow:          atomic.Bool{},
		cancel:          cancel,
		closeSourceOnce: closeSourceOnce,
		doneChan:        make(chan struct{}),
	}

	for _, opt := range opts {
		opt(fw)
	}

	fw.follow.Store(true)

	go func() {
		defer close(fw.doneChan)
		defer cancel()
		defer func() {
			if fw.closeSourceOnCancel {
				fw.closeSourceOnce()
			}
		}()

		buf := make([]byte, defaultBufferSize)
		timer := time.NewTimer(0)
		timer.Stop() // Stop the timer initially
		defer timer.Stop()

		sawData := false
		remainingStopRetries := fw.noDataStopRetries

		for {
			read := 0
			var readErr error

			read, readErr = source.Read(buf)
			if read > 0 {
				sawData = true

				out, writeErr := dest.Write(buf[:read])
				if writeErr != nil {
					fw.err.Store(writeErr)
					return
				}

				if out != read {
					fw.err.Store(io.ErrShortWrite)
					return
				}
			}

			if readErr != nil && readErr != io.EOF {
				if followCtx.Err() != nil {
					return
				}
				fw.err.Store(readErr)
				return
			}

			if read <= 0 || readErr == io.EOF {
				if !fw.follow.Load() {
					// If we have seen data, stop immediately.
					// If we have never seen data, use remaining stop retries before giving up.
					if sawData || remainingStopRetries <= 0 {
						return
					}
					remainingStopRetries--
				}

				// We didn't have any data to read, so wait for a bit and try again
				// Use a timer to wait for next read attempt
				// Wait a bit and try reading from the stream again.
				timer.Reset(readRetryInterval)
				select {
				case <-followCtx.Done():
					// Cancellation, so stop what we're doing
					return
				case <-timer.C:
					if followCtx.Err() != nil {
						return
					}
					continue
				}
			}
		}
	}()

	return fw
}

func (fw *FollowWriter) StopFollow() {
	fw.follow.Store(false)
}

func (fw *FollowWriter) Err() error {
	rawVal := fw.err.Load()
	if rawVal == nil {
		return nil
	}
	return rawVal.(error)
}

func (fw *FollowWriter) Done() <-chan struct{} {
	return fw.doneChan
}

func (fw *FollowWriter) Cancel() {
	fw.cancel()
	if fw.closeSourceOnCancel {
		fw.closeSourceOnce()
	}
}
