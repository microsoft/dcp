// Copyright (c) Microsoft Corporation. All rights reserved.

package logs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const (
	defaultTestTimeout = 20 * time.Second
)

func getTempLogFilePath(t *testing.T) string {
	suffix, err := randdata.MakeRandomString(8)
	require.NoError(t, err)
	return filepath.Join(testutil.TestTempRoot(), fmt.Sprintf("%s-%s.log", t.Name(), suffix))
}

// Ensure that log watcher in follow mode run on empty file does not return any data.
func TestWatchLogsFollowEmptyFile(t *testing.T) {
	t.Parallel()

	tmpFilePath := getTempLogFilePath(t)
	tmpFile, tmpFileErr := usvc_io.OpenFile(tmpFilePath, os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, tmpFileErr)
	require.NoError(t, tmpFile.Close())
	t.Cleanup(func() { _ = os.Remove(tmpFilePath) })

	buf := testutil.NewBufferWriter()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	watcherDone := make(chan struct{})

	go func() {
		err := WatchLogs(ctx, tmpFilePath, buf, WatchLogOptions{Follow: true})
		require.NoError(t, err)
		close(watcherDone)
	}()

	// Give it a bit of time to start watching the file.
	time.Sleep(100 * time.Millisecond)

	cancel()
	<-watcherDone

	require.Empty(t, buf.Bytes())
}

// Ensure that log watcher in follow mode run on a pre-created file returns all contents of the file
func TestWatchLogsFollowWholeFile(t *testing.T) {
	t.Parallel()

	const content = "hello\nworld\n"
	tmpFilePath := getTempLogFilePath(t)
	tmpFile, tmpFileErr := usvc_io.OpenFile(tmpFilePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, tmpFileErr)
	n, tmpFileErr := tmpFile.WriteString(content)
	require.NoError(t, tmpFileErr)
	require.Equal(t, len(content), n)
	require.NoError(t, tmpFile.Close())
	t.Cleanup(func() { _ = os.Remove(tmpFilePath) })

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	buf := testutil.NewBufferWriter()
	watcherDone := make(chan struct{})

	go func() {
		err := WatchLogs(ctx, tmpFilePath, buf, WatchLogOptions{Follow: true})
		require.NoError(t, err)
		close(watcherDone)
	}()

	err := wait.PollUntilContextCancel(ctx, cleanupReadyPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
		return bytes.Equal(buf.Bytes(), []byte(content)), nil
	})
	require.NoError(t, err, "watcher did not emit expected content, buffer content is: %s", string(buf.Bytes()))
	cancel()
	<-watcherDone
}

// Ensure that log watcher in follow mode returns data as it is appended to the file.
// The test varies the timing between the writes and watcher creation, from the case when the watcher is created before the first write,
// to the case when the watcher is created after the last write.
func TestWatchLogsFollowGetsAllData(t *testing.T) {
	t.Parallel()

	var content = []string{"alpha\n", "beta\n", "gamma\n", "delta\n"}
	allContent := strings.Join(content, "")
	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	doWatchLogs := func(ctx context.Context, path string, buf *testutil.BufferWriter, done chan struct{}) {
		err := WatchLogs(ctx, path, buf, WatchLogOptions{Follow: true})
		require.NoError(t, err)
		close(done)
	}

	for try := 0; try < len(content)+1; try++ {
		tmpFilePath := getTempLogFilePath(t)
		tmpFile, tmpFileErr := usvc_io.OpenFile(tmpFilePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, osutil.PermissionOnlyOwnerReadWrite)
		require.NoError(t, tmpFileErr)
		t.Cleanup(func() {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFilePath)
		})

		tryCtx, cancel := context.WithCancel(testCtx)
		buf := testutil.NewBufferWriter()
		watcherDone := make(chan struct{})

		for write := 0; write < len(content); write++ {
			if try == write {
				// Start the watcher
				go doWatchLogs(tryCtx, tmpFilePath, buf, watcherDone)
			}

			n, err := tmpFile.WriteString(content[write])
			require.NoError(t, err)
			require.Equal(t, len(content[write]), n)
		}

		if try == len(content) {
			// Final case: start the watcher after ALL writes are done
			go doWatchLogs(tryCtx, tmpFilePath, buf, watcherDone)
		}

		// After all writes are done, the watcher should have emitted all the content
		err := wait.PollUntilContextCancel(tryCtx, cleanupReadyPollInterval, pollImmediately, func(_ context.Context) (bool, error) {
			return bytes.Equal(buf.Bytes(), []byte(allContent)), nil
		})
		require.NoError(t, err, "watcher did not emit expected content, buffer content is: %s", string(buf.Bytes()))

		cancel() // Cancel the watcher
		<-watcherDone
	}
}

func TestWatchLogsMissingFile(t *testing.T) {
	t.Parallel()

	tmpFilePath := getTempLogFilePath(t)
	// Do NOT create the file, that is the point of the test

	testCtx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	err := WatchLogs(testCtx, tmpFilePath, nopWriteCloser{}, WatchLogOptions{Follow: false})
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestWatchLogsNoFollowEmptyFile(t *testing.T) {
	t.Parallel()

	tmpFilePath := getTempLogFilePath(t)
	tmpFile, tmpFileErr := usvc_io.OpenFile(tmpFilePath, os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, tmpFileErr)
	require.NoError(t, tmpFile.Close())
	t.Cleanup(func() { _ = os.Remove(tmpFilePath) })

	buf := testutil.NewBufferWriter()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	err := WatchLogs(ctx, tmpFilePath, buf, WatchLogOptions{Follow: false})
	require.NoError(t, err)
	require.Empty(t, buf.Bytes())
}

func TestWatchLogsNoFollowWholeFile(t *testing.T) {
	t.Parallel()

	const content = "hello\nworld\n"
	tmpFilePath := getTempLogFilePath(t)
	tmpFile, tmpFileErr := usvc_io.OpenFile(tmpFilePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, tmpFileErr)
	n, tmpFileErr := tmpFile.WriteString(content)
	require.NoError(t, tmpFileErr)
	require.Equal(t, len(content), n)
	require.NoError(t, tmpFile.Close())
	t.Cleanup(func() { _ = os.Remove(tmpFilePath) })

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	buf := testutil.NewBufferWriter()

	err := WatchLogs(ctx, tmpFilePath, buf, WatchLogOptions{Follow: false})
	require.NoError(t, err)
	received := string(buf.Bytes())
	require.Equal(t, content, received, "watcher did not emit expected content, buffer content is: %s", received)
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}
func (nopWriteCloser) Close() error {
	return nil
}

var _ io.WriteCloser = nopWriteCloser{}
