package logger

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/stretchr/testify/require"
)

func TestResourceSink(t *testing.T) {
	t.Parallel()

	resourceId := "resource-sink-test"
	expectedResourceFilePath := GetResourceLogPath(resourceId)

	logger := New("resource-sink-log").WithResourceSink().WithName("resource-sink-log")
	log := logger.Logger

	defer ReleaseAllResourceLogs()

	require.NoFileExists(t, expectedResourceFilePath)

	log = log.WithValues(RESOURCE_LOG_STREAM_ID, resourceId)
	log.Info("This is a test log entry", "Key1", "Value1")

	logger.flush()

	// Ensure that the resource log file exists
	fileExistsErr := resiliency.RetryExponentialWithTimeout(t.Context(), 2*time.Second, func() error {
		_, statErr := os.Stat(expectedResourceFilePath)
		return statErr
	})
	require.NoError(t, fileExistsErr)

	require.FileExists(t, expectedResourceFilePath)

	file, fileErr := usvc_io.OpenFile(expectedResourceFilePath, os.O_RDONLY, 0)
	require.NoError(t, fileErr)

	contents, readErr := io.ReadAll(file)
	require.NoError(t, readErr)
	defer file.Close()

	require.Contains(t, string(contents), "This is a test log entry")
	require.Contains(t, string(contents), "{\"Key1\": \"Value1\"}")
}

func TestResourceSinkNoResourceId(t *testing.T) {
	t.Parallel()

	resourceId := "resource-sink-no-resource-id-test"
	expectedResourceFilePath := GetResourceLogPath(resourceId)

	logger := New("resource-sink-no-resource-id-log").WithResourceSink().WithName("resource-sink-no-resource-id-log")
	log := logger.Logger

	defer ReleaseAllResourceLogs()

	require.NoFileExists(t, expectedResourceFilePath)

	log = log.WithValues(RESOURCE_LOG_STREAM_ID, resourceId, "Key1", "Value1")
	log.Info("This is a resource with an id", "Key2", "Value2")
	log.Error(fmt.Errorf("error of some sort"), "This is an error record")

	// When we reset the resource ID we should no longer write to the file
	log = log.WithValues(RESOURCE_LOG_STREAM_ID, resourceId)
	log.Info("This log entry has no resource id", "Key3", "Value3")

	logger.flush()

	// Ensure that the resource log file exists
	fileExistsErr := resiliency.RetryExponentialWithTimeout(t.Context(), 2*time.Second, func() error {
		_, statErr := os.Stat(expectedResourceFilePath)
		return statErr
	})
	require.NoError(t, fileExistsErr)

	file, fileErr := usvc_io.OpenFile(expectedResourceFilePath, os.O_RDONLY, 0)
	require.NoError(t, fileErr)

	contents, readErr := io.ReadAll(file)
	require.NoError(t, readErr)
	defer file.Close()

	require.Contains(t, string(contents), "info\tresource-sink-no-resource-id-log\tThis is a resource with an id\t{\"Key1\": \"Value1\", \"Key2\": \"Value2\"}")
	require.Contains(t, string(contents), "error\tresource-sink-no-resource-id-log\tThis is an error record\t{\"Key1\": \"Value1\", \"error\": \"error of some sort\"}")
}

func TestMain(m *testing.M) {
	previousTempDir := tempDir
	newTempDir, err := os.MkdirTemp(os.TempDir(), "resource-sink-test-")
	if err != nil {
		panic(err)
	}
	tempDir = newTempDir
	defer func() { tempDir = previousTempDir }()

	code := m.Run()

	if code == 0 {
		os.RemoveAll(tempDir)
	}

	os.Exit(code)
}
