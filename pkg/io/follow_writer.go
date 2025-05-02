package io

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

const (
	defaultBufferSize = 1024
	readRetryInterval = 200 * time.Millisecond
)

type FollowWriter struct {
	err      atomic.Value
	follow   atomic.Bool
	cancel   context.CancelFunc
	doneChan chan struct{}
}

// Creates a FollowWriter that reads content from the reader source and writes it to the writer destination.
// Keeps trying to read new content even after EOF until StopFollow() is called, after which the next EOF
// received will cause the reader and writer to stop.
// If the source is an io.Closer, it will be closed when the FollowWriter is cancelled.
func NewFollowWriter(ctx context.Context, source io.Reader, dest io.Writer) *FollowWriter {
	followCtx, cancel := context.WithCancel(ctx)
	fw := &FollowWriter{
		err:      atomic.Value{},
		follow:   atomic.Bool{},
		cancel:   cancel,
		doneChan: make(chan struct{}),
	}

	fw.follow.Store(true)

	go func() {
		defer func() {
			// If the source can be closed, do so
			if closer, isCloser := source.(io.Closer); isCloser {
				closer.Close()
			}
		}()
		defer cancel()
		defer close(fw.doneChan)

		buf := make([]byte, defaultBufferSize)
		timer := time.NewTimer(0)
		timer.Stop() // Stop the timer initially
		defer timer.Stop()

		for {
			read := 0
			var readErr error

			read, readErr = source.Read(buf)
			if read > 0 {
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
				fw.err.Store(readErr)
				return
			}

			if read <= 0 || readErr == io.EOF {
				if !fw.follow.Load() {
					return
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
}
