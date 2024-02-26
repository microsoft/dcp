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

	"github.com/fsnotify/fsnotify"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
)

const (
	defaultBufferSize   = 4096 // Does not have to be, but it is the same size as bufio default buffer size (Go 1.22)
	maxFileWatchTimeout = 10 * time.Second
	minFileWatchTimeout = 100 * time.Millisecond
	fileWatchRetries    = 3
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

	// Defer watcher/timer creation until they are needed. We do not need to be notified about file changes
	// when we are still tranmitting already-existing contents to the destination.
	//
	// TODO: we should
	// 1. Store log files in a separate directory off of session directory
	// 2. Use a single file watcher over the whole logs directory
	// 3. Have individual log file watchers subscribe to the directory watcher for change notifications to their own files
	var watcher *fsnotify.Watcher
	var timer *time.Timer

	// If this is the first time we are watching a resource, there is a lot of opportunity for timing issues to occur.
	// We might be just spinning up the log capturing process, and the log source file might be empty.
	// This means first read will not get any data, and we set up a file notification watcher.
	// If the log data is small and gets written in one shot right after our first read, but before we set up the watcher,
	// we will miss the change event.
	// To work around this issue we will start with a small watch timeout and increase it exponentially to maxFileWatchTimeout.
	// After we get going, it is unlikely that we miss any change events, so we can keep the timeout at max.
	watchTimeout := minFileWatchTimeout

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

		if watcher == nil {
			var infraErr error
			watcher, timer, infraErr = createFileWatchInfra(path)
			if infraErr != nil {
				return infraErr
			}
			defer watcher.Close()
			defer timer.Stop()
			// Try again to read from log file as we might have missed the change event between last read and adding the watcher.
			continue
		}

		endReason := waitForLogChange(ctx, watcher, path, timer, watchTimeout)

		if endReason == waitEndReasonTimerExpired && watchTimeout < maxFileWatchTimeout {
			watchTimeout *= 2
			if watchTimeout > maxFileWatchTimeout {
				watchTimeout = maxFileWatchTimeout
			}
		}
	}
}

func createFileWatchInfra(path string) (*fsnotify.Watcher, *time.Timer, error) {
	watcher, watcherErr := fsnotify.NewWatcher()
	if watcherErr != nil {
		return nil, nil, fmt.Errorf("failed to create file watcher for log file '%s': %w", path, watcherErr)
	}

	watcherErr = watcher.Add(path)
	if watcherErr != nil {
		watcher.Close()
		return nil, nil, fmt.Errorf("failed to add log file '%s' to watcher: %w", path, watcherErr)
	}

	timer := time.NewTimer(minFileWatchTimeout)
	return watcher, timer, nil
}

type waitEndReason int

const (
	waitEndReasonFileChanged waitEndReason = iota
	waitEndReasonWatcherError
	waitEndReasonTimerExpired
	waitEndReasonContextCancelled
)

func waitForLogChange(
	ctx context.Context,
	watcher *fsnotify.Watcher,
	path string,
	timer *time.Timer,
	watchTimeout time.Duration,
) waitEndReason {
	fileWatchRetryCount := 0
	timer.Reset(watchTimeout)

	for {
		select {
		case <-ctx.Done():
			return waitEndReasonContextCancelled

		case we := <-watcher.Events:
			if (we.Op == fsnotify.Write || we.Op == fsnotify.Remove) && we.Name == path {
				return waitEndReasonFileChanged
			}

		case <-watcher.Errors:
			// Unlikely to happen, but the watcher might fail to deliver some events if there is a lot of file activity,
			// so we will retry a few times.
			if fileWatchRetryCount < fileWatchRetries {
				fileWatchRetryCount++
			} else {
				return waitEndReasonWatcherError
			}

		case <-timer.C:
			// Just in case the file watch event(s) do not arrive as expected, we will do some polling too.
			return waitEndReasonTimerExpired
		}
	}
}
