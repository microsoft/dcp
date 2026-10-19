/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logs

import (
	"io"
	"os"
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
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/randdata"
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

// Verifies that CaptureFile uses the existing stderr filename, appends without truncation,
// and rejects unsupported capture sources.
func TestCaptureFileAppendAndSourceSelection(t *testing.T) {
	t.Parallel()
	uid, uidErr := randdata.MakeRandomString(8)
	require.NoError(t, uidErr)
	resource := &apiv1.Container{ObjectMeta: metav1.ObjectMeta{Name: "capture", UID: types.UID(uid)}}
	capture, captureErr := NewCaptureFile(resource, logstream.LogStreamSourceStartupStderr)
	require.NoError(t, captureErr)
	t.Cleanup(func() { require.NoError(t, capture.Close(CaptureFileDisposeOnClose)) })
	require.Equal(t, "capture_starterr_"+string(uid), filepath.Base(capture.Path))
	_, firstWriteErr := capture.Writer.Write([]byte("first\n"))
	require.NoError(t, firstWriteErr)
	require.NoError(t, capture.Close(CaptureFileCloseDefault))
	reopened, reopenErr := NewCaptureFile(resource, logstream.LogStreamSourceStartupStderr)
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
	require.ErrorContains(t, sourceErr, "unknown startup log source")
}
