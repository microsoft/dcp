// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
)

const (
	defaultBufferSize    = 4096 // Does not have to be, but it is the same size as bufio default buffer size (Go 1.22)
	logReadRetryInterval = 400 * time.Millisecond
)

type WatchLogOptions struct {
	Follow bool
}

// WatchLogs watches a log file on disk and sends its contents to supplied writer
func WatchLogs(ctx context.Context, path string, dest io.WriteCloser, opts WatchLogOptions) error {
	if path == "" {
		return fmt.Errorf("log file path is empty")
	}
	if dest == nil {
		return fmt.Errorf("destination writer is nil")
	}

	src, fileErr := usvc_io.OpenFile(path, os.O_RDONLY, 0)
	if fileErr != nil {
		return fmt.Errorf("failed to open log file '%s': %w", path, fileErr)
	}
	defer src.Close()

	if !opts.Follow {
		// This is easy--we are going to just copy the file as-is into the destination.
		// If early cancellation is desired, it should be done by the destination writer returning an error.
		_, copyErr := io.Copy(dest, src)
		closeErr := dest.Close()
		return errors.Join(copyErr, closeErr)
	}

	// The harder case--we might hit EOF repeatedly, and we need to wait for new data to appear.
	srcReader := bufio.NewReader(src)
	buf := make([]byte, defaultBufferSize)
	defer dest.Close()

	// We experimented with file change notification libraries like fsnotify (github.com/fsnotify/fsnotify),
	// but ultimately decided to rely on simple polling. The reasons are:
	// - fsnotify does not work reliably on Windows
	// - We deal with at most a handful of log files, and they are modified and read by our own, single process,
	//   which means the likelyhood of having the file contents cached in memory by the OS is very high,
	//   and the cost of making a read() call to check for new data is very low.
	timer := time.NewTimer(0)

	for {
		if ctx.Err() != nil {
			// Log watching cancellation is a normal condition. Do not return/log an error.
			return nil
		}

		n, readErr := srcReader.Read(buf)

		if n > 0 {
			_, writeErr := dest.Write(buf[:n])
			if writeErr != nil {
				if errors.Is(writeErr, io.ErrClosedPipe) {
					// This is normal if the client has disconnected.
					return nil
				} else {
					return fmt.Errorf("failed to write contents of a log file '%s' to destination: %w", path, writeErr)
				}
			}
		}

		// The writing may involve network I/O and can take a while. Let's check for cancellation again.
		if ctx.Err() != nil {
			return nil
		}

		if readErr == nil {
			continue
		}

		if readErr != io.EOF {
			return fmt.Errorf("failed to read log file '%s': %w", path, readErr)
		}

		// Wait a bit and try reading from the file again.
		timer.Reset(logReadRetryInterval)
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			continue
		}
	}
}
