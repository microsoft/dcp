/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	stdslices "slices"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// This values acts as a special key for any logger that has a resource sink enabled. If the first argument to WithValues is this key, the second argument
	// will be treated as a resource log stream ID. In this case neither of the first two arguments will be included in the final log entries, but the resource
	// sink will track the resource ID value and use it to route a copy of log entries to a separate log file named based on the resource ID (i.e. resource-<resource_id>.log).
	// This log file will contain only the log entries partaining to that specific resource ID, allowing system logs pertaining to a given resource to be retrieved in isolation.
	RESOURCE_LOG_STREAM_ID = "resource_log_stream_id"
)

var (
	resourceLoggerLock     = &sync.Mutex{}
	resourceLoggerDisabled = &atomic.Bool{}
	resourceSinks          = map[string]*resourceFileSink{}
	tempDir                = usvc_io.DcpTempDir()
)

type resourceFileSink struct {
	file   *os.File
	logger logr.Logger
	flush  func()
}

func GetResourceLogPath(resourceId string) string {
	if resourceId == "" {
		return ""
	}

	return filepath.Join(tempDir, fmt.Sprintf("resource-%s.log", resourceId))
}

func ReleaseResourceLog(resourceId string) {
	resourceLoggerLock.Lock()
	defer resourceLoggerLock.Unlock()

	if sink, found := resourceSinks[resourceId]; found {
		// Flush any buffered log entries to the file
		sink.flush()
		// We're making a best effort to close the file, but don't care if the operation failed
		_ = sink.file.Close()
		delete(resourceSinks, resourceId)
	}
}

func ReleaseAllResourceLogs() {
	resourceLoggerLock.Lock()
	defer resourceLoggerLock.Unlock()

	resourceLoggerDisabled.Store(true)

	wg := &sync.WaitGroup{}
	wg.Add(len(resourceSinks))

	for resourceId, sink := range resourceSinks {
		go func(resourceId string, sink *resourceFileSink) {
			defer wg.Done()
			// Flush any buffered log entries to the file
			sink.flush()
			// We're making a best effort to close the file, but don't care if the operation failed
			_ = sink.file.Close()
		}(resourceId, sink)
	}

	// Clear out all resource sinks
	resourceSinks = map[string]*resourceFileSink{}

	wg.Wait()
}

type resourceSink struct {
	resourceName string
	resourceId   string
	values       []any
	atomicLevel  zap.AtomicLevel
	innerSink    logr.LogSink
}

func newResourceSink(atomicLevel zap.AtomicLevel, innerSink logr.LogSink) *resourceSink {
	sink := &resourceSink{
		atomicLevel: zap.NewAtomicLevel(),
		innerSink:   innerSink,
	}
	sink.atomicLevel.SetLevel(atomicLevel.Level())

	return sink
}

func (s *resourceSink) Flush() {
	resourceLoggerLock.Lock()
	defer resourceLoggerLock.Unlock()

	wg := &sync.WaitGroup{}
	wg.Add(len(resourceSinks))

	for _, sink := range resourceSinks {
		go func(rfs *resourceFileSink) {
			defer wg.Done()
			rfs.flush()
		}(sink)
	}

	wg.Wait()
}

// Enabled implements logr.LogSink.
func (s *resourceSink) Enabled(level int) bool {
	return s.innerSink.Enabled(level)
}

// Error implements logr.LogSink.
func (s *resourceSink) Error(err error, msg string, keysAndValues ...any) {
	s.innerSink.Error(err, msg, keysAndValues...)

	s.writeResourceError(s.resourceId, err, msg, keysAndValues...)
}

// Info implements logr.LogSink.
func (s *resourceSink) Info(level int, msg string, keysAndValues ...any) {
	s.innerSink.Info(level, msg, keysAndValues...)

	s.writeResourceInfo(s.resourceId, level, msg, keysAndValues...)
}

// Init implements logr.LogSink.
func (s *resourceSink) Init(info logr.RuntimeInfo) {
	s.innerSink.Init(info)
}

// WithName implements logr.LogSink.
func (s *resourceSink) WithName(name string) logr.LogSink {
	if s.resourceName != "" {
		name = s.resourceName + "." + name
	}

	newSink := &resourceSink{
		resourceName: name,
		resourceId:   s.resourceId,
		values:       s.values,
		atomicLevel:  zap.NewAtomicLevel(),
		innerSink:    s.innerSink.WithName(name),
	}
	newSink.atomicLevel.SetLevel(s.atomicLevel.Level())

	return newSink
}

// WithValues implements logr.LogSink.
func (s *resourceSink) WithValues(keysAndValues ...any) logr.LogSink {
	resourceId := s.resourceId
	// For performance reasons, we only check the first argument against the RESOURCE_LOG_STREAM_ID key
	// and only when there are two or more arguments passed in.
	if len(keysAndValues) >= 2 && keysAndValues[0] == RESOURCE_LOG_STREAM_ID {
		resourceId = keysAndValues[1].(string)
		keysAndValues = keysAndValues[2:]
	}

	newSink := resourceSink{
		resourceName: s.resourceName,
		resourceId:   resourceId,
		atomicLevel:  zap.NewAtomicLevel(),
		innerSink:    s.innerSink.WithValues(keysAndValues...),
	}
	newSink.atomicLevel.SetLevel(s.atomicLevel.Level())
	values := stdslices.Clone(s.values)
	values = append(values, keysAndValues...)
	newSink.values = values

	return &newSink
}

func (s *resourceSink) writeResourceError(resourceId string, err error, msg string, keysAndValues ...any) {
	sink := s.getSink(resourceId)

	if sink != nil {
		sink.logger.WithValues(s.values...).GetSink().Error(err, msg, keysAndValues...)
	}
}

func (s *resourceSink) writeResourceInfo(resourceId string, level int, msg string, keysAndValues ...any) {
	sink := s.getSink(resourceId)

	if sink != nil {
		sink.logger.WithValues(s.values...).GetSink().Info(level, msg, keysAndValues...)
	}
}

func (s *resourceSink) getSink(resourceId string) *resourceFileSink {
	if resourceId == "" {
		// Nothing to write if resource ID is empty
		return nil
	}

	if resourceLoggerDisabled.Load() {
		// Resource logging is disabled, nothing to do
		return nil
	}

	resourceLoggerLock.Lock()
	defer resourceLoggerLock.Unlock()

	if resourceLoggerDisabled.Load() {
		// Resource logging is disabled, nothing to do
		return nil
	}

	sink, found := resourceSinks[resourceId]
	var sinkErr error
	if !found {
		// Create a new sink if one doesn't exist
		sink, sinkErr = s.newResourceFileSink(resourceId)
		if sinkErr == nil {
			resourceSinks[resourceId] = sink
		}
	}

	return sink
}

func (s *resourceSink) newResourceFileSink(resourceId string) (*resourceFileSink, error) {
	file, err := usvc_io.OpenFile(GetResourceLogPath(resourceId), os.O_RDWR|os.O_CREATE|os.O_APPEND, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		return nil, err
	}

	// Format console output to be human readable
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	// Honor Windows line endings for logs if appropriate
	if runtime.GOOS == "windows" {
		encoderConfig.LineEnding = string(osutil.CRLF())
	}
	consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)

	zapLogger := zap.New(zapcore.NewCore(consoleEncoder, zapcore.Lock(file), s.atomicLevel))

	return &resourceFileSink{
		file:   file,
		logger: zapr.NewLogger(zapLogger).WithName(s.resourceName),
		flush:  func() { _ = zapLogger.Sync() },
	}, nil
}

var _ logr.LogSink = (*resourceSink)(nil)
