// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
)

const (
	pollImmediately          = true // Don't wait before polling for the first time
	cleanupReadyPollInterval = 200 * time.Millisecond
)

// LogDescriptor is a struct that holds information about logs beloning to a DCP resource
// (e.g. Container, Executable etc).
type LogDescriptor struct {
	ResourceName types.NamespacedName
	ResourceUID  types.UID

	// The context for resource logs, to be used by log watchers.
	// When the resource is deleted, the context is cancelled and all log watchers terminate.
	Context       context.Context
	CancelContext context.CancelFunc

	lock          *sync.Mutex // Protects the fields below
	stdOut        *logfile
	stdErr        *logfile
	consumerCount uint32 // Number of active log watchers.
	lastUsed      time.Time
	disposed      bool
}

// Creates new LogDescriptor.
// Note: this is separated from log file creation so make sure that NewLogDescriptor() never fails,
// and as a result, the LogDescriptor is easy to use as a value type in a syncmap.
func NewLogDescriptor(
	ctx context.Context,
	cancel context.CancelFunc,
	resourceName types.NamespacedName,
	resourceUID types.UID,
) *LogDescriptor {
	return &LogDescriptor{
		ResourceName:  resourceName,
		ResourceUID:   resourceUID,
		Context:       ctx,
		CancelContext: cancel,
		lastUsed:      time.Now(),
		lock:          &sync.Mutex{},
	}
}

// Returns information about destination files for capturing resource logs, creating them as necessary.
// The returned bool indicates whether the files were created by this call.
func (l *LogDescriptor) EnableLogCapturing(logsFolder string) (io.WriteCloser, io.WriteCloser, bool, error) {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.disposed {
		return nil, nil, false, fmt.Errorf("this LogDescriptor (for %s) has been disposed", l.ResourceName.String())
	}

	l.lastUsed = time.Now()

	if l.stdOut != nil && l.stdErr != nil {
		return l.stdOut, l.stdErr, false, nil
	}

	if err := l.createLogFiles(logsFolder); err != nil {
		return nil, nil, false, err
	}

	return l.stdOut, l.stdErr, true, nil
}

// Notifies the log descriptor that another log watcher has started to use the log files.
// Returns the paths to the log files, or an error if the log descriptor was disposed.
func (l *LogDescriptor) LogConsumerStarting() (string, string, error) {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.disposed {
		return "", "", fmt.Errorf("this LogDescriptor (for %s) has been disposed", l.ResourceName.String())
	}

	l.lastUsed = time.Now()
	l.consumerCount++
	return l.stdOut.Id(), l.stdErr.Id(), nil
}

// Notifies the log descriptor that a log watcher has stopped using the log files.
func (l *LogDescriptor) LogConsumerStopped() {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.consumerCount > 0 {
		l.consumerCount--
		l.lastUsed = time.Now()
	}
}

func (l *LogDescriptor) IsDisposed() bool {
	l.lock.Lock()
	defer l.lock.Unlock()

	return l.disposed
}

// Returns the number of active log watchers and last use time.
func (l *LogDescriptor) Usage() (uint32, time.Time) {
	l.lock.Lock()
	defer l.lock.Unlock()

	return l.consumerCount, l.lastUsed
}

// Waits for all log watchers to stop (with a context + extra time serving as deadline)
// and then deletes the log files (best effort).
func (l *LogDescriptor) Dispose(ctx context.Context, extraTime time.Duration) error {
	l.lock.Lock()
	if l.disposed {
		l.lock.Unlock()
		return nil
	}
	l.disposed = true
	l.CancelContext()
	l.lock.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	if extraTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, extraTime)
		defer cancel()
	}

	return l.doCleanup(ctx)
}

func (l *LogDescriptor) doCleanup(deadline context.Context) error {
	_ = wait.PollUntilContextCancel(deadline, cleanupReadyPollInterval, pollImmediately, func(ctx context.Context) (bool, error) {
		l.lock.Lock()
		defer l.lock.Unlock()

		if l.consumerCount > 0 {
			return false, nil
		}

		if l.stdOut != nil && !l.stdOut.IsClosed() {
			return false, nil
		}

		if l.stdErr != nil && !l.stdErr.IsClosed() {
			return false, nil
		}

		return true, nil
	})

	stdOutPath := l.stdOut.Id()
	stdErrPath := l.stdErr.Id()
	stdOutRemoveErr := os.Remove(stdOutPath)
	stdErrRemoveErr := os.Remove(stdErrPath)
	return errors.Join(stdOutRemoveErr, stdErrRemoveErr)
}

func (l *LogDescriptor) createLogFiles(logsFolder string) error {
	// To avoid file name conflicts when log watching is stopped and quickly started again for the same resource,
	// we include a random suffix in the file names.
	suffix, randErr := randdata.MakeRandomString(4)
	if randErr != nil {
		// Should never happen
		return fmt.Errorf("could not generate random suffix for log file names for resource %s: %w", l.ResourceName.String(), randErr)
	}

	stdOutFileName := fmt.Sprintf("%s_out_%s_%s", l.ResourceName.Name, l.ResourceUID, string(suffix))
	stdOutPath := filepath.Join(logsFolder, stdOutFileName)
	stdOut, stdOutErr := usvc_io.OpenFile(stdOutPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdOutErr != nil {
		// If we cannot create stdout or stderr file, this descriptor is pretty much unusable.
		// We consider is disposed.
		l.disposed = true
		return fmt.Errorf("could not create stdout log file for resource %s: %w", l.ResourceName.String(), stdOutErr)
	}

	stdErrFileName := fmt.Sprintf("%s_err_%s_%s", l.ResourceName.Name, l.ResourceUID, string(suffix))
	stdErrPath := filepath.Join(logsFolder, stdErrFileName)
	stdErr, stdErrErr := usvc_io.OpenFile(stdErrPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdErrErr != nil {
		l.disposed = true
		stdOutCloseErr := stdOut.Close()
		stdOutRemoveErr := os.Remove(stdOutPath)
		return errors.Join(
			fmt.Errorf("could not create stderr log file for resource %s: %w", l.ResourceName.String(), stdErrErr),
			stdOutCloseErr,
			stdOutRemoveErr,
		)
	}

	l.stdOut = newLogFile(stdOut, stdOutPath)
	l.stdErr = newLogFile(stdErr, stdErrPath)
	return nil
}
