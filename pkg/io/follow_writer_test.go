package io_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const (
	defaultTestTimeout       = 20 * time.Second
	cleanupReadyPollInterval = 200 * time.Millisecond
)

// Ensure that follow writer in follow mode run on empty file does not return any data.
func TestFollowWriterFollowEmptyFile(t *testing.T) {
	t.Parallel()

	tmpReader, tmpWriter := usvc_io.NewBufferedPipe()

	buf := testutil.NewBufferWriter()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)

	writer := usvc_io.NewFollowWriter(ctx, tmpReader, buf)

	// Give it a bit of time to start watching the file.
	time.Sleep(100 * time.Millisecond)

	cancel()
	require.NoError(t, tmpWriter.Close())
	<-writer.Done()

	require.Empty(t, buf.Bytes())
}

// Ensure that follow writer in follow mode run on a pre-created file returns all contents of the file
func TestFollowWriterFollowWholeFile(t *testing.T) {
	t.Parallel()

	const content = "hello\nworld\n"
	tmpReader, tmpWriter := usvc_io.NewBufferedPipe()
	n, tmpWriteErr := tmpWriter.Write([]byte(content))
	require.NoError(t, tmpWriteErr)
	require.Equal(t, len(content), n)

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	buf := testutil.NewBufferWriter()

	writer := usvc_io.NewFollowWriter(ctx, tmpReader, buf)

	err := wait.PollUntilContextCancel(ctx, cleanupReadyPollInterval, true, func(_ context.Context) (bool, error) {
		return bytes.Equal(buf.Bytes(), []byte(content)), nil
	})
	require.NoError(t, err, "follow writer did not emit expected content, buffer content is: %s", string(buf.Bytes()))
	writer.StopFollow()
	require.NoError(t, tmpWriter.Close())
	<-writer.Done()
}

// Ensure that follow writer in follow mode returns data as it is appended to the file.
// The test varies the timing between the writes and writer creation, from the case when the writer is created before the first write,
// to the case when the writer is created after the last write.
func TestFollowWriterFollowGetsAllData(t *testing.T) {
	t.Parallel()

	var content = []string{"alpha\n", "beta\n", "gamma\n", "delta\n"}
	allContent := strings.Join(content, "")
	testCtx, cancelTestCtx := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancelTestCtx()

	for try := 0; try < len(content)+1; try++ {
		tmpReader, tmpWriter := usvc_io.NewBufferedPipe()

		tryCtx, cancel := context.WithCancel(testCtx)
		buf := testutil.NewBufferWriter()

		var writer *usvc_io.FollowWriter

		for write := 0; write < len(content); write++ {
			if try == write {
				// Start the follow writer
				writer = usvc_io.NewFollowWriter(tryCtx, tmpReader, buf)
			}

			n, err := tmpWriter.Write([]byte(content[write]))
			require.NoError(t, err)
			require.Equal(t, len(content[write]), n)
		}

		require.NoError(t, tmpWriter.Close())

		if try == len(content) {
			writer = usvc_io.NewFollowWriter(tryCtx, tmpReader, buf)
		}

		// After all writes are done, the follow writer should have emitted all the content
		err := wait.PollUntilContextCancel(tryCtx, cleanupReadyPollInterval, true, func(_ context.Context) (bool, error) {
			return bytes.Equal(buf.Bytes(), []byte(allContent)), nil
		})
		require.NoError(t, err, "follow writer did not emit expected content, buffer content is: %s", string(buf.Bytes()))

		cancel() // Cancel the follow writer
		<-writer.Done()
	}
}

func TestFollowWriterStopsFollowingFile(t *testing.T) {
	t.Parallel()

	tmpReader, tmpWriter := usvc_io.NewBufferedPipe()
	require.NoError(t, tmpWriter.Close())

	buf := testutil.NewBufferWriter()
	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()

	writer := usvc_io.NewFollowWriter(ctx, tmpReader, buf)
	writer.StopFollow()

	<-writer.Done()

	require.Empty(t, buf.Bytes())
}

func TestFollowWriterNoFollowWholeFile(t *testing.T) {
	t.Parallel()

	const content = "hello\nworld\n"
	tmpReader, tmpWriter := usvc_io.NewBufferedPipe()
	n, tmpWriteErr := tmpWriter.Write([]byte(content))
	require.NoError(t, tmpWriteErr)
	require.Equal(t, len(content), n)
	require.NoError(t, tmpWriter.Close())

	ctx, cancel := testutil.GetTestContext(t, defaultTestTimeout)
	defer cancel()
	buf := testutil.NewBufferWriter()

	writer := usvc_io.NewFollowWriter(ctx, tmpReader, buf)
	writer.StopFollow()

	<-writer.Done()

	received := string(buf.Bytes())
	require.Equal(t, content, received, "follow writer did not emit expected content, buffer content is: %s", received)
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}
func (nopWriteCloser) Close() error {
	return nil
}

var _ io.WriteCloser = nopWriteCloser{}
