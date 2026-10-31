/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2resourcelogs

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/logstream"
	"github.com/microsoft/dcp/pkg/randdata"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that logical process streaming uses only its projected immediate capture snapshot
// and completes through the existing kind-wide lifecycle callback.
func TestLogicalProcessLogsUseProjectedSnapshot(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	uid, uidErr := randdata.MakeRandomString(8)
	require.NoError(t, uidErr)
	physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "physical", UID: types.UID(uid)}}
	stdout, stdoutErr := logs.NewCaptureFile(physical, logstream.LogStreamSourceStdout)
	require.NoError(t, stdoutErr)
	defer func() { require.NoError(t, stdout.Close(logs.CaptureFileDisposeOnClose)) }()
	stderr, stderrErr := logs.NewCaptureFile(physical, logstream.LogStreamSourceStderr)
	require.NoError(t, stderrErr)
	defer func() { require.NoError(t, stderr.Close(logs.CaptureFileDisposeOnClose)) }()
	_, writeErr := stdout.Writer.Write([]byte("projected-process-output\n"))
	require.NoError(t, writeErr)
	require.NoError(t, stdout.Close(logs.CaptureFileCloseDefault))
	require.NoError(t, stderr.Close(logs.CaptureFileCloseDefault))
	physical.Status.StdoutFile, physical.Status.StderrFile = stdout.Path, stderr.Path
	logical := &apiv2.LogicalProcess{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", UID: "logical-uid"},
		Status: apiv2.LogicalProcessStatus{
			Phase: apiv2.LogicalProcessPhasePending, RuntimeLogs: physical.GetLogSource(),
		},
	}
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	reader, writer := io.Pipe()
	defer func() { require.NoError(t, reader.Close()) }()
	defer func() { require.NoError(t, writer.Close()) }()
	status, done, streamErr := streamer.StreamLogs(ctx, writer, logical, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusStreaming, status)
	output := make([]byte, len("projected-process-output\n"))
	_, readErr := io.ReadFull(reader, output)
	require.NoError(t, readErr)
	require.Equal(t, "projected-process-output\n", string(output))
	logical.Status.RuntimeLogs.Complete = true
	streamer.OnResourceUpdated(apiv2.ResourceWatcherEvent{Type: watch.Modified, Object: logical}, logr.Discard())
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("logical process follower did not finish")
	}
}

// Verifies that process endpoints reject startup sources, wait for pending capture,
// and return completed empty output for adopted processes without capture.
func TestProcessLogAvailabilityAndSources(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "physical", UID: "physical-uid"}}
	logical := &apiv2.LogicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "logical", UID: "logical-uid"}}
	for _, source := range []string{"startup_stdout", "startup_stderr"} {
		require.True(t, apierrors.IsBadRequest(streamer.Preflight(physical, &apiv2.ResourceLogOptions{Source: source})))
		require.True(t, apierrors.IsBadRequest(streamer.Preflight(logical, &apiv2.ResourceLogOptions{Source: source})))
	}
	var output bytes.Buffer
	status, _, streamErr := streamer.StreamLogs(ctx, &output, logical, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusNotReady, status)
	physical.Status.LogsComplete = true
	logical.Status.RuntimeLogs = physical.GetLogSource()
	status, _, streamErr = streamer.StreamLogs(ctx, &output, logical, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusDone, status)
	require.Empty(t, output.String())
}
