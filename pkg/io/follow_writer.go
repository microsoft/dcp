package io

import (
	"bufio"
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
	err      error
	follow   atomic.Bool
	cancel   context.CancelFunc
	done     atomic.Bool
	doneChan chan struct{}
}

// Creates a FollowWriter that reads content from the reader source and writes it to the writer destination.
// Keeps trying to read new content even after EOF until StopFollow() is called, after which the next EOF
// received will cause the reader and writer to stop.
func NewFollowWriter(ctx context.Context, source io.Reader, dest io.Writer) *FollowWriter {
	followCtx, cancel := context.WithCancel(ctx)
	fw := &FollowWriter{
		follow:   atomic.Bool{},
		cancel:   cancel,
		done:     atomic.Bool{},
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
		// Goroutine that handles following the source and writing to the destination
		reader := bufio.NewReader(source)
		buf := make([]byte, defaultBufferSize)
		timer := time.NewTimer(0)
		timer.Stop()

		for {
			if fw.done.Load() {
				return
			}

			read := 0
			var readErr error
			// Peek to see if there's any data to read as reader.Read can potentially block
			if _, peekErr := reader.Peek(1); peekErr == nil {
				read, readErr = reader.Read(buf)

				if read > 0 {
					out, writeErr := dest.Write(buf[:read])
					if writeErr != nil {
						fw.finish(writeErr)
						return
					}

					if out != read {
						fw.finish(io.ErrShortWrite)
						return
					}
				}

				if readErr != nil && readErr != io.EOF {
					fw.finish(readErr)
					return
				}
			}

			if read <= 0 || readErr == io.EOF {
				if !fw.follow.Load() {
					fw.finish(nil)
					return
				}

				// We didn't have any data to read, so wait for a bit and try again
				// Use a timer to wait for next read attempt
				// Wait a bit and try reading from the stream again.
				timer.Reset(readRetryInterval)
				select {
				case <-followCtx.Done():
					// Cancellation, so stop what we're doing
					timer.Stop()
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
	return fw.err
}

func (fw *FollowWriter) Done() <-chan struct{} {
	return fw.doneChan
}

func (fw *FollowWriter) Cancel() {
	fw.cancel()
}

func (fw *FollowWriter) finish(err error) {
	if fw.done.CompareAndSwap(false, true) {
		fw.err = err
	}
}
