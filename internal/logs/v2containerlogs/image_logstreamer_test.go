/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v2containerlogs

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	apiv2 "github.com/microsoft/dcp/api/v2"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that startup preflight validates image capture identity and paths without disabling system logs.
func TestImageStartupPreflightValidatesCapturePath(t *testing.T) {
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "namespace", UID: "container-uid"},
		Status: apiv2.LogicalContainerStatus{StartupLogs: &apiv2.LogSource{
			Name: "image", UID: "image-uid", StdoutFile: filepath.Join(t.TempDir(), "image_startout_image-uid"),
			StderrFile: filepath.Join(t.TempDir(), "image_starterr_image-uid"), Available: true,
		}},
	}
	opts := &apiv2.ResourceLogOptions{Source: "startup_stdout"}
	require.NoError(t, streamer.Preflight(container, opts))
	container.Status.StartupLogs.StdoutFile = filepath.Join(t.TempDir(), "unrelated-file")
	require.True(t, apierrors.IsInternalError(streamer.Preflight(container, opts)))
	require.NoError(t, streamer.Preflight(container, &apiv2.ResourceLogOptions{Source: "system"}))
	image := &apiv2.PhysicalContainerImage{ObjectMeta: metav1.ObjectMeta{Name: "image", UID: "image-uid"}}
	require.NoError(t, streamer.Preflight(image, &apiv2.ResourceLogOptions{}))
	require.True(t, apierrors.IsBadRequest(streamer.Preflight(image, opts)))
}

// Verifies that runtime and startup descriptors use the same file-following and completion handling,
// driven solely by updates to the watched logical resource.
func TestLogSourcesShareSessionCompletion(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	folder := t.TempDir()
	stdoutPath := filepath.Join(folder, "source_startout_source-uid")
	stderrPath := filepath.Join(folder, "source_starterr_source-uid")
	stdout, stdoutErr := usvc_io.EnsureEmptyFile(stdoutPath, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stdoutErr)
	writer := usvc_io.NewTimestampWriter(stdout)
	_, writeErr := writer.Write([]byte("captured-output\n"))
	require.NoError(t, writeErr)
	require.NoError(t, writer.Close())
	stderr, stderrErr := usvc_io.EnsureEmptyFile(stderrPath, osutil.PermissionOnlyOwnerReadWrite)
	require.NoError(t, stderrErr)
	require.NoError(t, stderr.Close())
	source := &apiv2.LogSource{
		Name: "source", UID: "source-uid", StdoutFile: stdoutPath, StderrFile: stderrPath, Available: true,
	}
	container := &apiv2.LogicalContainer{
		ObjectMeta: metav1.ObjectMeta{Name: "container", Namespace: "namespace", UID: "logical-uid"},
		Status:     apiv2.LogicalContainerStatus{RuntimeLogs: source.DeepCopy(), StartupLogs: source.DeepCopy()},
	}
	streamer := NewLogStreamer(logr.Discard())
	defer func() { require.NoError(t, streamer.Dispose()) }()
	runtimeReader, runtimeWriter := io.Pipe()
	startupReader, startupWriter := io.Pipe()
	defer func() { require.NoError(t, runtimeReader.Close()) }()
	defer func() { require.NoError(t, startupReader.Close()) }()
	_, runtimeDone, runtimeErr := streamer.StreamLogs(ctx, runtimeWriter, container, &apiv2.ResourceLogOptions{Follow: true}, logr.Discard())
	require.NoError(t, runtimeErr)
	_, startupDone, startupErr := streamer.StreamLogs(ctx, startupWriter, container,
		&apiv2.ResourceLogOptions{Source: "startup_stdout", Follow: true}, logr.Discard())
	require.NoError(t, startupErr)
	buffer := make([]byte, len("captured-output\n"))
	_, runtimeReadErr := io.ReadFull(runtimeReader, buffer)
	require.NoError(t, runtimeReadErr)
	require.Equal(t, "captured-output\n", string(buffer))
	_, startupReadErr := io.ReadFull(startupReader, buffer)
	require.NoError(t, startupReadErr)
	require.Equal(t, "captured-output\n", string(buffer))
	container.Status.RuntimeLogs.Complete = true
	container.Status.StartupLogs.Complete = true
	streamer.OnResourceUpdated(apiv2.ResourceWatcherEvent{Type: watch.Modified, Object: container}, logr.Discard())
	for _, done := range []<-chan struct{}{runtimeDone, startupDone} {
		select {
		case <-done:
		case <-ctx.Done():
			require.FailNow(t, "source session did not complete")
		}
	}
	require.NoError(t, runtimeWriter.Close())
	require.NoError(t, startupWriter.Close())
}
