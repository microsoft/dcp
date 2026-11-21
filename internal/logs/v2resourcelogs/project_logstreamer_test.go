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

// Verifies that ProjectProcess and ProjectContainer wait for runtime and image capture, rejects process startup sources,
// and completes empty output when stopped or failed before physical capture exists.
func TestProjectLogAvailabilityAndSources(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	for _, container := range []bool{false, true} {
		var resource apiv2.ProjectObject
		meta := metav1.ObjectMeta{Name: "project", UID: "project-uid"}
		if container {
			resource = &apiv2.ProjectContainer{ObjectMeta: meta, Spec: apiv2.ProjectContainerSpec{
				Container: apiv2.ProjectContainerTemplate{LogicalContainerConfig: apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "image"}}},
			}}
		} else {
			resource = &apiv2.ProjectProcess{ObjectMeta: meta, Spec: apiv2.ProjectProcessSpec{
				Process: apiv2.ProjectProcessTemplate{LogicalProcessConfig: apiv2.LogicalProcessConfig{ExecutablePath: "program"}},
			}}
		}
		for _, source := range []string{"stdout", "stderr", "startup_stdout", "startup_stderr"} {
			opts := &apiv2.ResourceLogOptions{Source: source, Follow: true}
			preflightErr := streamer.Preflight(resource, opts)
			if !container && (source == "startup_stdout" || source == "startup_stderr") {
				require.True(t, apierrors.IsBadRequest(preflightErr))
				continue
			}
			require.NoError(t, preflightErr)
			var output bytes.Buffer
			resource.GetProjectStatus().Phase = apiv2.ProjectPhasePending
			status, _, streamErr := streamer.StreamLogs(ctx, &output, resource, opts, logr.Discard())
			require.NoError(t, streamErr)
			require.Equal(t, logstream.ResourceStreamStatusNotReady, status)
			for _, phase := range []apiv2.ProjectPhase{apiv2.ProjectPhaseStopped, apiv2.ProjectPhaseFailed} {
				resource.GetProjectStatus().Phase = phase
				status, _, streamErr = streamer.StreamLogs(ctx, &output, resource, opts, logr.Discard())
				require.NoError(t, streamErr)
				require.Equal(t, logstream.ResourceStreamStatusDone, status)
				require.Empty(t, output.String())
			}
		}
	}
}

// Verifies that an exited ProjectProcess waits for incomplete capture, delivers later output,
// and finishes followers only after the physical process capture completes.
func TestProjectProcessExitedLogsWaitForCapture(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	uid, uidErr := randdata.MakeRandomString(8)
	require.NoError(t, uidErr)
	physical := &apiv2.PhysicalProcess{ObjectMeta: metav1.ObjectMeta{Name: "physical", UID: types.UID(uid)}}
	stdout, stdoutErr := logs.NewCaptureFile(physical, logstream.LogStreamSourceStdout)
	require.NoError(t, stdoutErr)
	defer func() { require.NoError(t, stdout.Close(logs.CaptureFileDisposeOnClose)) }()

	project := &apiv2.ProjectProcess{ObjectMeta: metav1.ObjectMeta{Name: "project", UID: types.UID("project-" + string(uid))}}
	project.Status.Phase = apiv2.ProjectPhaseExited
	project.Status.RuntimeLogs = physical.GetLogSource()
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	options := &apiv2.ResourceLogOptions{Follow: true}
	var empty bytes.Buffer
	status, _, streamErr := streamer.StreamLogs(ctx, &empty, project, options, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusNotReady, status)

	physical.Status.StdoutFile = stdout.Path
	project.Status.RuntimeLogs = physical.GetLogSource()
	_, initialWriteErr := stdout.Writer.Write([]byte("before-exit\n"))
	require.NoError(t, initialWriteErr)
	reader, writer := io.Pipe()
	defer func() { require.NoError(t, reader.Close()) }()
	defer func() { require.NoError(t, writer.Close()) }()
	status, done, streamErr := streamer.StreamLogs(ctx, writer, project, options, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusStreaming, status)
	first := make([]byte, len("before-exit\n"))
	_, initialReadErr := io.ReadFull(reader, first)
	require.NoError(t, initialReadErr)
	require.Equal(t, "before-exit\n", string(first))
	streamer.OnResourceUpdated(apiv2.ResourceWatcherEvent{Type: watch.Modified, Object: project}, logr.Discard())
	select {
	case <-done:
		t.Fatal("Project exit finished an incomplete capture")
	default:
	}
	_, laterWriteErr := stdout.Writer.Write([]byte("after-exit\n"))
	require.NoError(t, laterWriteErr)
	later := make([]byte, len("after-exit\n"))
	_, laterReadErr := io.ReadFull(reader, later)
	require.NoError(t, laterReadErr)
	require.Equal(t, "after-exit\n", string(later))
	require.NoError(t, stdout.Close(logs.CaptureFileCloseDefault))
	physical.Status.LogsComplete = true
	project.Status.RuntimeLogs = physical.GetLogSource()
	streamer.OnResourceUpdated(apiv2.ResourceWatcherEvent{Type: watch.Modified, Object: project}, logr.Discard())
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("completed Project capture did not finish its follower")
	}
}
