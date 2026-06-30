/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logger

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/resiliency"
)

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
		file, fileErr := usvc_io.OpenFile(expectedResourceFilePath, os.O_RDONLY, 0)
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
		file, fileErr := usvc_io.OpenFile(expectedResourceFilePath, os.O_RDONLY, 0)
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
