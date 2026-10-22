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

// Verifies that physical execution completion, deletion, and completed unavailable logical sources
// finish followers through the existing kind-wide lifecycle callbacks.
func TestContainerExecLifecycleEventsCompleteLogicalFollowers(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		event   watch.EventType
		logical bool
	}{
		{name: "physical-completion", event: watch.Modified},
		{name: "physical-deletion", event: watch.Deleted},
		{name: "logical-source-unavailable", event: watch.Modified, logical: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, time.Minute)
			defer cancel()
			uid, uidErr := randdata.MakeRandomString(8)
			require.NoError(t, uidErr)
			physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "execution", UID: types.UID(uid)}}
			stdout, stdoutErr := logs.NewCaptureFile(physical, logstream.LogStreamSourceStdout)
			require.NoError(t, stdoutErr)
			defer func() { require.NoError(t, stdout.Close(logs.CaptureFileDisposeOnClose)) }()
			stderr, stderrErr := logs.NewCaptureFile(physical, logstream.LogStreamSourceStderr)
			require.NoError(t, stderrErr)
			defer func() { require.NoError(t, stderr.Close(logs.CaptureFileDisposeOnClose)) }()
			_, writeErr := stdout.Writer.Write([]byte("execution-output\n"))
			require.NoError(t, writeErr)
			require.NoError(t, stdout.Close(logs.CaptureFileCloseDefault))
			require.NoError(t, stderr.Close(logs.CaptureFileCloseDefault))
			physical.Status.StdoutFile, physical.Status.StderrFile = stdout.Path, stderr.Path
			logical := &apiv2.LogicalContainerExec{
				ObjectMeta: metav1.ObjectMeta{Name: "logical", UID: "logical-uid"},
				Status:     apiv2.LogicalContainerExecStatus{RuntimeLogs: physical.GetLogSource()},
			}
			streamer := NewLogStreamer(logr.Discard())
			defer func() { require.NoError(t, streamer.Dispose()) }()
			reader, writer := io.Pipe()
			defer func() { require.NoError(t, reader.Close()) }()
			defer func() { require.NoError(t, writer.Close()) }()
			status, done, streamErr := streamer.StreamLogs(ctx, writer, logical, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
			require.NoError(t, streamErr)
			require.Equal(t, logstream.ResourceStreamStatusStreaming, status)
			initial := make([]byte, len("execution-output\n"))
			_, readErr := io.ReadFull(reader, initial)
			require.NoError(t, readErr)
			require.Equal(t, "execution-output\n", string(initial))
			physical.Status.LogsComplete = true
			event := apiv2.ResourceWatcherEvent{Type: testCase.event, Object: physical}
			if testCase.logical {
				logical.Status.Phase = apiv2.ContainerExecPhaseFailed
				logical.Status.RuntimeLogs = &apiv2.LogSource{Name: physical.Name, UID: physical.UID, Complete: true}
				event.Object = logical
			}
			streamer.OnResourceUpdated(event, logr.Discard())
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("execution lifecycle event did not finish the logical follower")
			}
			if !testCase.logical {
				require.False(t, logical.Status.RuntimeLogs.Complete)
			}
		})
	}
}

// Verifies that container execution log endpoints wait for capture, reject image startup sources,
// and complete empty output when execution fails before capture is available.
func TestContainerExecLogAvailabilityAndSources(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	physical := &apiv2.PhysicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "physical", UID: "physical-uid"}}
	logical := &apiv2.LogicalContainerExec{ObjectMeta: metav1.ObjectMeta{Name: "logical", UID: "logical-uid"}}
	for _, source := range []string{"startup_stdout", "startup_stderr"} {
		require.True(t, apierrors.IsBadRequest(streamer.Preflight(physical, &apiv2.ResourceLogOptions{Source: source})))
		require.True(t, apierrors.IsBadRequest(streamer.Preflight(logical, &apiv2.ResourceLogOptions{Source: source})))
	}
	var output bytes.Buffer
	status, _, streamErr := streamer.StreamLogs(ctx, &output, logical, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusNotReady, status)
	physical.Status.LogsComplete = true
	logical.Status.Phase = apiv2.ContainerExecPhaseFailed
	status, _, streamErr = streamer.StreamLogs(ctx, &output, logical, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusDone, status)
	status, _, streamErr = streamer.StreamLogs(ctx, &output, physical, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusDone, status)
	require.Empty(t, output.String())
}
