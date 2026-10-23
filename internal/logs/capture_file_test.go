/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logs

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/logstream"
	internal_testutil "github.com/microsoft/dcp/internal/testutil"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that CaptureFile preserves V1 filenames, numbering, timestamps, paragraph separation,
// and idempotent close-then-delete behavior.
func TestCaptureFileFormattingAndLifecycle(t *testing.T) {
	t.Parallel()
	uid, uidErr := randdata.MakeRandomString(8)
	require.NoError(t, uidErr)
	resource := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: "capture", UID: types.UID(uid)}}
	capture, captureErr := NewCaptureFile(resource, logstream.LogStreamSourceStartupStdout)
	require.NoError(t, captureErr)
	t.Cleanup(func() { require.NoError(t, capture.Close(CaptureFileDisposeOnClose)) })
	require.Equal(t, "capture_startout_"+string(uid), filepath.Base(capture.Path))
	_, firstWriteErr := capture.Writer.Write([]byte("first\n"))
	require.NoError(t, firstWriteErr)
	capture.Writer.NewParagraph()
	_, secondWriteErr := capture.Writer.Write([]byte("second\n"))
	require.NoError(t, secondWriteErr)
	require.NoError(t, capture.Close(CaptureFileCloseDefault))
	require.NoError(t, capture.Close(CaptureFileCloseDefault))
	require.FileExists(t, capture.Path)

	raw, readErr := os.ReadFile(capture.Path)
	require.NoError(t, readErr)
	firstLine := strings.SplitN(string(raw), " ", 3)
	require.Len(t, firstLine, 3)
	require.Equal(t, "1", firstLine[0])
	_, timestampErr := time.Parse(time.RFC3339, firstLine[1])
	require.NoError(t, timestampErr)
	file, openErr := usvc_io.OpenFileReadOnly(capture.Path)
	require.NoError(t, openErr)
	reader := usvc_io.NewTimestampAwareReader(file, usvc_io.TimestampAwareReaderOptions{LineNumbers: true})
	contents, contentsErr := io.ReadAll(reader)
	require.NoError(t, contentsErr)
	require.NoError(t, reader.Close())
	require.Equal(t, "1 first\n2 "+string(osutil.LineSep())+"3 second\n", string(contents))

	capture.CloseAndLog(CaptureFileDisposeOnClose, logr.Discard())
	require.NoError(t, capture.Close(CaptureFileDisposeOnClose))
	require.NoFileExists(t, capture.Path)
}

// Verifies that direct capture uses inherited file handles rather than DCP-owned pipes,
// allowing retained process output after capture closure and DCP-side lifetime cancellation.
func TestCaptureFileDirectProcessOutputSurvivesCaptureLifetime(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	uid, uidErr := randdata.MakeRandomString(8)
	require.NoError(t, uidErr)
	resource := &apiv1.Executable{ObjectMeta: metav1.ObjectMeta{Name: "direct", UID: types.UID(uid)}}
	stdout, stdoutErr := NewCaptureFile(resource, logstream.LogStreamSourceStdout, CaptureFileDirectOutput)
	require.NoError(t, stdoutErr)
	defer func() { require.NoError(t, stdout.Close(CaptureFileDisposeOnClose)) }()
	stderr, stderrErr := NewCaptureFile(resource, logstream.LogStreamSourceStderr, CaptureFileDirectOutput)
	require.NoError(t, stderrErr)
	defer func() { require.NoError(t, stderr.Close(CaptureFileDisposeOnClose)) }()
	toolPath, toolErr := internal_testutil.GetTestToolPath("delay")
	require.NoError(t, toolErr)
	executor := process.NewOSExecutor(logr.Discard())
	defer executor.Dispose()
	command := exec.Command(toolPath, "-d", "1s")
	command.Stdout, command.Stderr = stdout.DirectFile, stderr.DirectFile
	lifetime, cancelLifetime := context.WithCancel(ctx)
	defer cancelLifetime()
	exited := make(chan struct{})
	handle, startWait, startErr := executor.StartProcess(context.WithoutCancel(lifetime), command,
		process.ProcessExitHandlerFunc(func(process.Pid_t, int32, error) { close(exited) }), process.CreationFlagsNone, nil)
	require.NoError(t, startErr)
	defer func() {
		cleanupExecutor := process.NewOSExecutor(logr.Discard())
		defer cleanupExecutor.Dispose()
		stopErr := cleanupExecutor.StopProcess(context.WithoutCancel(ctx), handle)
		require.True(t, stopErr == nil || process.IsProcessGoneErr(stopErr), "%v", stopErr)
	}()
	startWait()
	require.NoError(t, stdout.Close(CaptureFileCloseDefault))
	require.NoError(t, stderr.Close(CaptureFileCloseDefault))
	cancelLifetime()
	executor.Dispose()
	require.NoError(t, executor.CheckProcessRunning(handle))
	select {
	case <-exited:
	case <-ctx.Done():
		t.Fatal("retained process did not complete")
	}
	output, readErr := os.ReadFile(stdout.Path)
	require.NoError(t, readErr)
	require.Contains(t, string(output), "Ran to completion")
}

// Verifies that CaptureFile captures ordinary stderr, appends without truncation,
// and rejects unsupported capture sources.
func TestCaptureFileAppendAndSourceSelection(t *testing.T) {
	t.Parallel()
	uid, uidErr := randdata.MakeRandomString(8)
	require.NoError(t, uidErr)
	resource := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: "capture", UID: types.UID(uid)}}
	capture, captureErr := NewCaptureFile(resource, logstream.LogStreamSourceStderr)
	require.NoError(t, captureErr)
	t.Cleanup(func() { require.NoError(t, capture.Close(CaptureFileDisposeOnClose)) })
	require.Equal(t, "capture_err_"+string(uid), filepath.Base(capture.Path))
	_, firstWriteErr := capture.Writer.Write([]byte("first\n"))
	require.NoError(t, firstWriteErr)
	require.NoError(t, capture.Close(CaptureFileCloseDefault))
	reopened, reopenErr := NewCaptureFile(resource, logstream.LogStreamSourceStderr)
	require.NoError(t, reopenErr)
	t.Cleanup(func() { require.NoError(t, reopened.Close(CaptureFileCloseDefault)) })
	require.Equal(t, capture.Path, reopened.Path)
	_, nextWriteErr := reopened.Writer.Write([]byte("second\n"))
	require.NoError(t, nextWriteErr)
	require.NoError(t, reopened.Close(CaptureFileCloseDefault))
	contents, readErr := os.ReadFile(capture.Path)
	require.NoError(t, readErr)
	require.Contains(t, string(contents), "first\n")
	require.Contains(t, string(contents), "second\n")
	_, sourceErr := NewCaptureFile(resource, logstream.LogStreamSourceSystem)
	require.ErrorContains(t, sourceErr, "unknown capture log source")
}

// Verifies that CaptureFile retries a transient removal failure instead of caching it after
// the obstruction disappears, and treats repeated successful disposal as idempotent.
func TestCaptureFileRetriesFailedDisposal(t *testing.T) {
	t.Parallel()
	uid, uidErr := randdata.MakeRandomString(8)
	require.NoError(t, uidErr)
	resource := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: "dispose-retry", UID: types.UID(uid)}}
	capture, captureErr := NewCaptureFile(resource, logstream.LogStreamSourceStdout)
	require.NoError(t, captureErr)
	require.NoError(t, capture.Close(CaptureFileCloseDefault))
	saved := capture.Path + ".saved"
	obstruction := filepath.Join(capture.Path, "busy")
	t.Cleanup(func() {
		_ = os.Remove(obstruction)
		_ = os.Remove(capture.Path)
		_ = os.Remove(saved)
	})
	require.NoError(t, os.Rename(capture.Path, saved))
	require.NoError(t, os.Mkdir(capture.Path, 0700))
	require.NoError(t, usvc_io.WriteFile(obstruction, []byte("busy"), osutil.PermissionOnlyOwnerReadWrite))
	require.Error(t, capture.Close(CaptureFileDisposeOnClose))
	require.NoError(t, os.Remove(obstruction))
	require.NoError(t, os.Remove(capture.Path))
	require.NoError(t, os.Rename(saved, capture.Path))
	require.NoError(t, capture.Close(CaptureFileDisposeOnClose))
	require.NoError(t, capture.Close(CaptureFileDisposeOnClose))
	require.NoFileExists(t, capture.Path)
}
