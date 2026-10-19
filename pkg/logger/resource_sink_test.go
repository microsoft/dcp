/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/resiliency"
)

type countingLogSink struct {
	errorCount int
}

// Verifies that WithAdditionalResourceStream preserves context and primary binding, writes each
// destination once, and leaves the original logger and other resource streams independent.
func TestAdditionalResourceStreamPreservesScopedLogging(t *testing.T) {
	t.Parallel()
	folder := t.TempDir()
	primaryID := t.Name() + "-physical"
	logicalID := t.Name() + "-logical"
	defer ReleaseResourceLogsInFolder(folder)
	core, entries := observer.New(zap.DebugLevel)
	sink := newResourceSink(zap.NewAtomicLevelAt(zap.DebugLevel), zapr.NewLogger(zap.New(core)).GetSink(), folder)
	base := logr.New(sink).WithName("container").WithValues("namespace", "test-namespace")
	primary := base.WithValues(RESOURCE_LOG_STREAM_ID, primaryID)
	derived, deriveErr := WithAdditionalResourceStream(primary, logicalID)
	require.NoError(t, deriveErr)
	repeated, repeatErr := WithAdditionalResourceStream(derived, logicalID)
	require.NoError(t, repeatErr)
	scoped := repeated.WithName("worker").WithValues("operation", "start")
	scoped.Info("shared-info", "detail", "value")
	scoped.Error(fmt.Errorf("runtime failure"), "shared-error")
	base.Info("base-only")
	primary.Info("primary-only")
	ReleaseResourceLog(primaryID)
	parent := base.WithName("logical").WithValues(RESOURCE_LOG_STREAM_ID, logicalID)
	parent.Info("parent-only")
	rebound := derived.WithValues(RESOURCE_LOG_STREAM_ID, logicalID)
	rebound.Info("rebound")
	sink.Flush()

	readLog := func(resourceID string) string {
		file, openErr := usvc_io.OpenFileReadOnly(makeResourceLogPath(resourceID, folder))
		require.NoError(t, openErr)
		contents, readErr := io.ReadAll(file)
		closeErr := file.Close()
		require.NoError(t, readErr)
		require.NoError(t, closeErr)
		return string(contents)
	}
	physicalContents := readLog(primaryID)
	logicalContents := readLog(logicalID)
	require.Equal(t, 1, strings.Count(physicalContents, "shared-info"))
	require.Equal(t, 1, strings.Count(logicalContents, "shared-info"))
	require.Equal(t, 1, strings.Count(physicalContents, "shared-error"))
	require.Equal(t, 1, strings.Count(logicalContents, "shared-error"))
	require.Contains(t, physicalContents, "container.worker")
	require.Contains(t, logicalContents, "container.worker")
	require.Contains(t, logicalContents, "container.logical")
	require.Contains(t, logicalContents, `"namespace": "test-namespace"`)
	require.Contains(t, logicalContents, `"operation": "start"`)
	require.Contains(t, physicalContents, "primary-only")
	require.NotContains(t, logicalContents, "primary-only")
	require.Contains(t, logicalContents, "parent-only")
	require.NotContains(t, physicalContents, "parent-only")
	require.Equal(t, 1, strings.Count(logicalContents, "rebound"))
	require.NotContains(t, physicalContents, "base-only")
	require.NotContains(t, logicalContents, "base-only")
	require.Equal(t, 6, entries.Len())
	require.Equal(t, "container.worker", entries.All()[0].LoggerName)
}

// Verifies that WithAdditionalResourceStream supports a filtering sink without bypassing it
// and reports invalid or unsupported stream bindings explicitly.
func TestAdditionalResourceStreamHonorsFilter(t *testing.T) {
	t.Parallel()
	folder := t.TempDir()
	defer ReleaseResourceLogsInFolder(folder)
	sink := newResourceSink(zap.NewAtomicLevel(), &countingLogSink{}, folder)
	filtered := logr.New(newFilterSink("ignored", 10, sink)).WithValues(RESOURCE_LOG_STREAM_ID, t.Name()+"-physical")
	derived, deriveErr := WithAdditionalResourceStream(filtered, t.Name()+"-logical")
	require.NoError(t, deriveErr)
	derived.Error(fmt.Errorf("filtered"), "ignored")
	require.NoFileExists(t, makeResourceLogPath(t.Name()+"-logical", folder))
	require.NoFileExists(t, makeResourceLogPath(t.Name()+"-physical", folder))
	_, emptyErr := WithAdditionalResourceStream(filtered, "")
	require.ErrorContains(t, emptyErr, "must not be empty")
	_, unsupportedErr := WithAdditionalResourceStream(logr.Discard(), "logical")
	require.ErrorContains(t, unsupportedErr, "does not contain a resource sink")
}

// Verifies that nested resource sinks report file creation failures without deadlocking
// and retain per-resource backoff while unrelated resource logging remains available.
func TestAdditionalResourceStreamReportsFileCreationFailures(t *testing.T) {
	t.Parallel()
	invalidFolder := filepath.Join(t.TempDir(), "missing")
	validFolder := t.TempDir()
	primaryID, additionalID := t.Name()+"-primary", t.Name()+"-additional"
	defer ReleaseResourceLog(primaryID)
	defer ReleaseResourceLog(additionalID)
	core, entries := observer.New(zap.DebugLevel)
	root := zapr.NewLogger(zap.New(core))
	sink := newResourceSink(zap.NewAtomicLevelAt(zap.DebugLevel), root.GetSink(), invalidFolder)
	primary := logr.New(sink).WithValues(RESOURCE_LOG_STREAM_ID, primaryID)
	derived, deriveErr := WithAdditionalResourceStream(primary, additionalID)
	require.NoError(t, deriveErr)

	derived.Error(fmt.Errorf("operation failed"), "original failure")
	require.Equal(t, 1, entries.FilterMessage("original failure").Len())
	failures := entries.FilterMessage("Could not create resource log file").All()
	require.Len(t, failures, 2)
	resourceIDs := []any{failures[0].ContextMap()["ResourceID"], failures[1].ContextMap()["ResourceID"]}
	require.ElementsMatch(t, []any{primaryID, additionalID}, resourceIDs)
	for _, failure := range failures {
		require.Contains(t, failure.ContextMap()["error"], invalidFolder)
	}

	derived.Error(fmt.Errorf("operation failed again"), "subsequent failure")
	require.Equal(t, 1, entries.FilterMessage("subsequent failure").Len())
	require.Equal(t, 2, entries.FilterMessage("Could not create resource log file").Len())

	ReleaseResourceLog(additionalID)
	healthySink := newResourceSink(zap.NewAtomicLevelAt(zap.DebugLevel), root.GetSink(), validFolder)
	healthy := logr.New(healthySink).WithValues(RESOURCE_LOG_STREAM_ID, additionalID)
	healthy.Info("healthy resource output")
	ReleaseResourceLog(primaryID)
	derived.Error(fmt.Errorf("primary failed again"), "failure with cached healthy destination")
	require.Equal(t, 3, entries.FilterMessage("Could not create resource log file").Len())
	healthySink.Flush()
	contents, readErr := os.ReadFile(makeResourceLogPath(additionalID, validFolder))
	require.NoError(t, readErr)
	require.Contains(t, string(contents), "healthy resource output")
	require.Contains(t, string(contents), "failure with cached healthy destination")
	require.Contains(t, string(contents), "Could not create resource log file")
}

func (*countingLogSink) Init(logr.RuntimeInfo) {}

func (*countingLogSink) Enabled(int) bool {
	return true
}

func (*countingLogSink) Info(int, string, ...any) {}

func (sink *countingLogSink) Error(error, string, ...any) {
	sink.errorCount++
}

func (sink *countingLogSink) WithValues(...any) logr.LogSink {
	return sink
}

func (sink *countingLogSink) WithName(string) logr.LogSink {
	return sink
}

func TestResourceSink(t *testing.T) {
	t.Parallel()

	resourceIdSuffix, suffixErr := randdata.MakeRandomString(8)
	require.NoError(t, suffixErr)
	resourceId := "resource-sink-test" + string(resourceIdSuffix)
	expectedResourceFilePath := GetResourceLogPath(resourceId)

	logger := New("resource-sink-log").WithResourceSink().WithName("resource-sink-log")
	log := logger.Logger

	require.NoFileExists(t, expectedResourceFilePath)

	log = log.WithValues(RESOURCE_LOG_STREAM_ID, resourceId)
	log.Info("This is a test log entry", "Key1", "Value1")

	logger.Flush()
	defer ReleaseResourceLog(resourceId)

	// Ensure that the resource log file exists
	fileExistsErr := resiliency.RetryExponentialWithTimeout(t.Context(), 10*time.Second, func() error {
		_, statErr := os.Stat(expectedResourceFilePath)
		return statErr
	})
	require.NoError(t, fileExistsErr)

	defer func() { _ = os.Remove(expectedResourceFilePath) }()

	// logger.flush() does not guarantee that subsequent reads will see all the data immediately
	require.EventuallyWithTf(t, func(c *assert.CollectT) {
		file, fileErr := usvc_io.OpenFileReadOnly(expectedResourceFilePath)
		require.NoError(c, fileErr)
		if fileErr != nil {
			return
		}
		defer func() { _ = file.Close() }()

		contents, readErr := io.ReadAll(file)
		require.NoError(c, readErr)
		if readErr != nil {
			return
		}

		require.Contains(c, string(contents), "This is a test log entry")
		require.Contains(c, string(contents), "{\"Key1\": \"Value1\"}")
	}, 10*time.Second, 200*time.Millisecond, "Expected to find log entry in resource log file")
}

func TestResourceSinkNoResourceId(t *testing.T) {
	t.Parallel()

	resourceIdSuffix, suffixErr := randdata.MakeRandomString(8)
	require.NoError(t, suffixErr)
	resourceId := "resource-sink-no-resource-id-test" + string(resourceIdSuffix)
	expectedResourceFilePath := GetResourceLogPath(resourceId)

	logger := New("resource-sink-no-resource-id-log").WithResourceSink().WithName("resource-sink-no-resource-id-log")
	log := logger.Logger

	require.NoFileExists(t, expectedResourceFilePath)

	log = log.WithValues(RESOURCE_LOG_STREAM_ID, resourceId, "Key1", "Value1")
	log.Info("This is a resource with an id", "Key2", "Value2")
	log.Error(fmt.Errorf("error of some sort"), "This is an error record")

	// When we do not use the log with resource stream ID, we should not write to the resource log file.
	log = logger.Logger
	log.Info("This log entry has no resource id", "Key3", "Value3")

	logger.Flush()
	defer ReleaseResourceLog(resourceId)

	// Ensure that the resource log file exists
	fileExistsErr := resiliency.RetryExponentialWithTimeout(t.Context(), 10*time.Second, func() error {
		_, statErr := os.Stat(expectedResourceFilePath)
		return statErr
	})
	require.NoError(t, fileExistsErr)

	defer func() { _ = os.Remove(expectedResourceFilePath) }()

	// logger.flush() does not guarantee that subsequent reads will see all the data immediately
	require.EventuallyWithTf(t, func(c *assert.CollectT) {
		file, fileErr := usvc_io.OpenFileReadOnly(expectedResourceFilePath)
		require.NoError(c, fileErr)
		if fileErr != nil {
			return
		}
		defer func() { _ = file.Close() }()

		contents, readErr := io.ReadAll(file)
		require.NoError(c, readErr)
		if readErr != nil {
			return
		}

		require.Contains(c, string(contents), "info\tresource-sink-no-resource-id-log\tThis is a resource with an id\t{\"Key1\": \"Value1\", \"Key2\": \"Value2\"}")
		require.Contains(c, string(contents), "error\tresource-sink-no-resource-id-log\tThis is an error record\t{\"Key1\": \"Value1\", \"error\": \"error of some sort\"}")
	}, 10*time.Second, 200*time.Millisecond, "Expected to find a data and a log entry in resource log file")
}

func TestReleaseResourceLogsInFolder(t *testing.T) {
	t.Parallel()

	folderToRelease := t.TempDir()
	folderToKeep := t.TempDir()
	resourceIdSuffix, suffixErr := randdata.MakeRandomString(8)
	require.NoError(t, suffixErr)
	releasedResourceId := "resource-sink-release-folder-test-" + string(resourceIdSuffix)
	keptResourceId := "resource-sink-keep-folder-test-" + string(resourceIdSuffix)
	releasedResourceFilePath := makeResourceLogPath(releasedResourceId, folderToRelease)
	keptResourceFilePath := makeResourceLogPath(keptResourceId, folderToKeep)

	releasedLogger := New("resource-sink-release-folder-log").WithResourceSinkInto(folderToRelease)
	keptLogger := New("resource-sink-keep-folder-log").WithResourceSinkInto(folderToKeep)
	defer ReleaseResourceLog(keptResourceId)

	releasedLogger.Logger.WithValues(RESOURCE_LOG_STREAM_ID, releasedResourceId).Info("release this resource log")
	keptLog := keptLogger.Logger.WithValues(RESOURCE_LOG_STREAM_ID, keptResourceId)
	keptLog.Info("keep this resource log")
	releasedLogger.Flush()
	keptLogger.Flush()

	require.FileExists(t, releasedResourceFilePath)
	require.FileExists(t, keptResourceFilePath)

	ReleaseResourceLogsInFolder(folderToRelease)
	require.NoError(t, os.RemoveAll(folderToRelease))

	keptLog.Info("resource logging is still enabled")
	keptLogger.Flush()
	contents, readErr := os.ReadFile(keptResourceFilePath)
	require.NoError(t, readErr)
	require.Contains(t, string(contents), "keep this resource log")
	require.Contains(t, string(contents), "resource logging is still enabled")
}

func TestResourceSinkResolvesRelativeFolder(t *testing.T) {
	t.Parallel()

	workingDir, workingDirErr := os.Getwd()
	require.NoError(t, workingDirErr)
	outputDir, outputDirErr := os.MkdirTemp(workingDir, "resource-sink-relative-folder-*")
	require.NoError(t, outputDirErr)
	defer func() {
		require.NoError(t, os.RemoveAll(outputDir))
	}()
	relativeOutputDir, relativePathErr := filepath.Rel(workingDir, outputDir)
	require.NoError(t, relativePathErr)
	resourceIdSuffix, suffixErr := randdata.MakeRandomString(8)
	require.NoError(t, suffixErr)
	resourceId := "resource-sink-relative-folder-" + string(resourceIdSuffix)

	log := New("resource-sink-relative-folder").WithResourceSinkInto(relativeOutputDir)
	log.Logger.WithValues(RESOURCE_LOG_STREAM_ID, resourceId).Info("relative resource log")
	log.Flush()
	defer ReleaseResourceLog(resourceId)

	expectedPath := makeResourceLogPath(resourceId, outputDir)
	require.EventuallyWithTf(t, func(c *assert.CollectT) {
		require.FileExists(c, expectedPath)
	}, 10*time.Second, 200*time.Millisecond, "Expected relative resource log path to be resolved")
}

func TestResourceSinkBacksOffAfterFileCreationFailure(t *testing.T) {
	resourceId := "resource-sink-backoff"
	invalidFolder := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, usvc_io.WriteFile(invalidFolder, []byte("content"), osutil.PermissionOnlyOwnerReadWrite))
	validFolder := t.TempDir()

	innerSink := &countingLogSink{}
	sink := newResourceSink(zap.NewAtomicLevel(), innerSink, invalidFolder)
	now := time.Date(2026, time.September, 10, 12, 0, 0, 0, time.UTC)
	originalNow := resourceSinkNow
	resourceSinkNow = func() time.Time {
		return now
	}
	t.Cleanup(func() {
		resourceSinkNow = originalNow
		ReleaseResourceLog(resourceId)
	})

	require.Nil(t, sink.getSink(resourceId))
	require.Equal(t, 1, innerSink.errorCount)

	sink.resourceLogFolderOverride = validFolder
	require.Nil(t, sink.getSink(resourceId))
	require.Equal(t, 1, innerSink.errorCount)
	require.NoFileExists(t, makeResourceLogPath(resourceId, validFolder))

	now = now.Add(resourceSinkRetryInterval)
	require.NotNil(t, sink.getSink(resourceId))
	require.Equal(t, 1, innerSink.errorCount)
	require.FileExists(t, makeResourceLogPath(resourceId, validFolder))

	ReleaseResourceLog(resourceId)
	resourceLoggerLock.Lock()
	_, sinkFound := resourceSinks[resourceId]
	_, failureFound := resourceSinkFailures[resourceId]
	resourceLoggerLock.Unlock()
	require.False(t, sinkFound)
	require.False(t, failureFound)
}
