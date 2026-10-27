/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2resourcelogs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv2 "github.com/microsoft/dcp/api/v2"
	"github.com/microsoft/dcp/internal/containers"
	"github.com/microsoft/dcp/internal/contextdata"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/logstream"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

type recordedLogSource struct{ captures int }

// Verifies that Pending logical containers with external image references finish startup requests
// empty, while inline image startup requests remain pending until their owned capture is available.
func TestLogicalStartupLogsRequireOwnedImage(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: "namespace", UID: "logical-uid"},
		Spec:       apiv2.LogicalContainerSpec{Container: &apiv2.LogicalContainerConfig{ImageRef: "external"}},
		Status:     apiv2.LogicalContainerStatus{Phase: apiv2.LogicalContainerPhasePending},
	}
	for _, source := range []string{"startup_stdout", "startup_stderr"} {
		var output bytes.Buffer
		status, done, streamErr := streamer.StreamLogs(ctx, &output, logical,
			&apiv2.ResourceLogOptions{Source: source, Follow: true}, logr.Discard())
		require.NoError(t, streamErr)
		require.Equal(t, logstream.ResourceStreamStatusDone, status)
		require.Nil(t, done)
		require.Empty(t, output.String())
	}
	logical.Spec.Container = &apiv2.LogicalContainerConfig{Image: &apiv2.PhysicalContainerImageConfig{Image: "owned"}}
	status, _, streamErr := streamer.StreamLogs(ctx, io.Discard, logical,
		&apiv2.ResourceLogOptions{Source: "startup_stdout", Follow: true}, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusNotReady, status)
}

type observedPipeWriter struct {
	*io.PipeWriter
	entered chan struct{}
	once    sync.Once
}

func (writer *observedPipeWriter) Write(contents []byte) (int, error) {
	writer.once.Do(func() { close(writer.entered) })
	return writer.PipeWriter.Write(contents)
}

func (source *recordedLogSource) CaptureContainerLogs(
	_ context.Context, _ string, stdout, stderr usvc_io.WriteSyncerCloser, _ containers.StreamContainerLogsOptions,
) error {
	source.captures++
	_, writeErr := usvc_io.NewTimestampWriter(stdout).Write([]byte("shared-output\n"))
	return errors.Join(writeErr, stdout.Close(), stderr.Close())
}

// Verifies that logical and physical consumers share one capture and that logical Pending
// does not hide output from a runtime container that has already started.
func TestContainerStreamersSharePhysicalCapture(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, contextdata.HostLifetimeContextKey, ctx)
	source := &recordedLogSource{}
	streamer := &resourceLogStreamer{
		sessions: make(map[types.UID]map[uint64]*logSession), source: source,
		captures: logs.NewLogDescriptorSet(ctx, t.TempDir(), logr.Discard()), log: logr.Discard(),
	}
	defer func() { require.NoError(t, streamer.Dispose()) }()
	started := metav1.NowMicro()
	physical := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "physical", Namespace: "namespace", UID: "physical-uid"},
		Status:     apiv2.PhysicalContainerStatus{ContainerID: "runtime-id", StartedAt: started, Phase: apiv2.PhysicalContainerPhaseRunning},
	}
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: physical.Namespace, UID: "logical-uid"},
		Status: apiv2.LogicalContainerStatus{
			PhysicalContainerRef: physical.Name, RuntimeLogs: physical.GetLogSource(),
			ContainerID: physical.Status.ContainerID, StartedAt: started, Phase: apiv2.LogicalContainerPhasePending,
		},
	}
	var logicalOutput, physicalOutput bytes.Buffer
	status, logicalDone, logicalErr := streamer.StreamLogs(ctx, &logicalOutput, logical, &apiv2.ResourceLogOptions{}, logr.Discard())
	require.NoError(t, logicalErr)
	require.Equal(t, logstream.ResourceStreamStatusStreaming, status)
	select {
	case <-logicalDone:
	case <-ctx.Done():
		require.FailNow(t, "logical stream did not finish")
	}
	_, physicalDone, physicalErr := streamer.StreamLogs(ctx, &physicalOutput, physical, &apiv2.ResourceLogOptions{}, logr.Discard())
	require.NoError(t, physicalErr)
	select {
	case <-physicalDone:
	case <-ctx.Done():
		require.FailNow(t, "physical stream did not finish")
	}
	require.Equal(t, "shared-output\n", logicalOutput.String())
	require.Equal(t, logicalOutput.String(), physicalOutput.String())
	require.Equal(t, 1, source.captures)
}

// Verifies that preflight rejects unsafe observed capture identities before opening files
// while keeping system diagnostics available for the resource.
func TestContainerLogPreflightRejectsUnsafeCaptureIdentity(t *testing.T) {
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	logical := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "logical", Namespace: "namespace", UID: "logical-uid"},
		Status: apiv2.LogicalContainerStatus{
			RuntimeLogs:          &apiv2.LogSource{Name: "physical", UID: "../outside", ContainerID: "runtime-id", Available: true},
			PhysicalContainerRef: "physical",
			ContainerID:          "runtime-id", StartedAt: metav1.NowMicro(),
		},
	}

	require.True(t, apierrors.IsInternalError(streamer.Preflight(logical, &apiv2.ResourceLogOptions{})))
	require.NoError(t, streamer.Preflight(logical, &apiv2.ResourceLogOptions{Source: "system"}))
}

// Verifies that terminal startup failures with a runtime identity can still expose runtime output
// even when no successful start timestamp was observed.
func TestContainerLogsRemainAvailableAfterFailedStart(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, contextdata.HostLifetimeContextKey, ctx)
	source := &recordedLogSource{}
	streamer := &resourceLogStreamer{
		sessions: make(map[types.UID]map[uint64]*logSession), source: source,
		captures: logs.NewLogDescriptorSet(ctx, t.TempDir(), logr.Discard()), log: logr.Discard(),
	}
	defer func() { require.NoError(t, streamer.Dispose()) }()
	physical := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "physical", Namespace: "namespace", UID: "failed-uid"},
		Status:     apiv2.PhysicalContainerStatus{ContainerID: "runtime-id", Phase: apiv2.PhysicalContainerPhaseFailed},
	}
	var output bytes.Buffer
	status, done, streamErr := streamer.StreamLogs(ctx, &output, physical, &apiv2.ResourceLogOptions{}, logr.Discard())
	require.NoError(t, streamErr)
	require.Equal(t, logstream.ResourceStreamStatusStreaming, status)
	select {
	case <-done:
	case <-ctx.Done():
		require.FailNow(t, "failed container log stream did not finish")
	}
	require.Equal(t, "shared-output\n", output.String())
	require.Equal(t, 1, source.captures)
}

// Verifies that disposing a streamer unblocks an active response write and joins its client
// session even when the HTTP consumer is not reading.
func TestContainerLogDisposeUnblocksResponseWriter(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, contextdata.HostLifetimeContextKey, ctx)
	streamer := &resourceLogStreamer{
		sessions: make(map[types.UID]map[uint64]*logSession), source: &recordedLogSource{},
		captures: logs.NewLogDescriptorSet(ctx, t.TempDir(), logr.Discard()), log: logr.Discard(),
	}
	defer func() { require.NoError(t, streamer.Dispose()) }()
	reader, pipe := io.Pipe()
	defer func() { require.NoError(t, reader.Close()) }()
	writer := &observedPipeWriter{PipeWriter: pipe, entered: make(chan struct{})}
	physical := &apiv2.PhysicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "physical", Namespace: "namespace", UID: "physical-uid"},
		Status:     apiv2.PhysicalContainerStatus{ContainerID: "runtime-id", StartedAt: metav1.NowMicro()},
	}
	_, done, streamErr := streamer.StreamLogs(ctx, writer, physical, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
	require.NoError(t, streamErr)
	select {
	case <-writer.entered:
	case <-ctx.Done():
		require.FailNow(t, "response writer did not receive output")
	}
	disposed := make(chan error, 1)
	go func() { disposed <- streamer.Dispose() }()
	select {
	case disposeErr := <-disposed:
		require.NoError(t, disposeErr)
	case <-ctx.Done():
		require.FailNow(t, "streamer disposal did not finish")
	}
	select {
	case <-done:
	default:
		require.FailNow(t, "disposal returned before the session finished")
	}
}
