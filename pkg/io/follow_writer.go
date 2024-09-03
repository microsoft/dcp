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
	readRetryInterval = 400 * time.Millisecond
)

type FollowWriter struct {
	err      error
	follow   atomic.Bool
	done     atomic.Bool
	doneChan chan struct{}
}

// Creates a FollowWriter that reads content from the reader source and writes it to the writer destination.
// Keeps trying to read new content even after EOF until StopFollow() is called, after which the next EOF
// received will cause the reader and writer to stop.
func NewFollowWriter(ctx context.Context, source io.Reader, dest io.Writer) *FollowWriter {
	fw := &FollowWriter{
		follow:   atomic.Bool{},
		done:     atomic.Bool{},
		doneChan: make(chan struct{}),
	}

	fw.follow.Store(true)

	go func() {
		defer close(fw.doneChan)
		// Goroutine that handles following the source and writing to the destination
		reader := bufio.NewReader(source)
		buf := make([]byte, defaultBufferSize)
		timer := time.NewTimer(0)
		defer timer.Stop()

		for {
			if fw.done.Load() {
				return
			}

			in, readErr := reader.Read(buf)

			if in > 0 {
				out, writeErr := dest.Write(buf[:in])
				if writeErr != nil {
					fw.finish(writeErr)
					return
				}

				if out != in {
					fw.finish(io.ErrShortWrite)
					return
				}
			}

			if readErr != nil && readErr != io.EOF {
				fw.finish(readErr)
				return
			}

			if in <= 0 || readErr == io.EOF {
				if !fw.follow.Load() {
					fw.finish(nil)
					return
				}

				// We didn't have any data to read, so wait for a bit and try again
				// Use a timer to wait for next read attempt
				// Wait a bit and try reading from the stream again.
				timer.Reset(readRetryInterval)
				select {
				case <-ctx.Done():
					// Cancellation, so stop what we're doing
					return
				case <-timer.C:
					if ctx.Err() != nil {
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

func (fw *FollowWriter) finish(err error) {
	if fw.done.CompareAndSwap(false, true) {
		fw.err = err
	}
}
