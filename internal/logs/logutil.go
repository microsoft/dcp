// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"context"
	"errors"
	"os"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/resiliency"
)

const (
	DefaultLogFileRemovalTimeout = 3 * time.Second

	// A delay before we cancel a "follow" log stream (either completely, or stop the follow mode)
	// in response to a change in the state of the object that is producing the logs.
	// This delay allows us to capture the last lines of logs that were produced just before the object changed state.
	FollowStreamCancellationDelay = 5 * time.Second
)

// RemoveWithRetry removes a file with exponential backoff retry.
//
// This function is handy for files that store logs. When an object exposing logs is being deleted,
// DCP API server closes all associated log streams, but this happens asynchronously,
// so we might need to wait a bit for the files to be released.
func RemoveWithRetry(ctx context.Context, path string) error {
	return resiliency.RetryExponentialWithTimeout(ctx, DefaultLogFileRemovalTimeout, func() error {
		removeErr := os.Remove(path)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			return nil
		} else {
			return removeErr
		}
	})
}

func ToTimestampReaderOptions(opts *apiv1.LogOptions) usvc_io.TimestampAwareReaderOptions {
	retval := usvc_io.TimestampAwareReaderOptions{
		Timestamps:  opts.Timestamps,
		LineNumbers: opts.LineNumbers,
	}
	if opts.Limit != nil {
		retval.Limit = *opts.Limit
	}
	if opts.Skip != nil {
		retval.Skip = *opts.Skip
	}
	return retval
}

func DelayCancelFollowStreams(streams []*usvc_io.FollowWriter, cancelStream func(*usvc_io.FollowWriter)) {
	go func() {
		time.Sleep(FollowStreamCancellationDelay)
		for _, stream := range streams {
			cancelStream(stream)
		}
	}()
}
