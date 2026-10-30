/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package stdiologs

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/logs"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that stdIoLogStreamer finite copies preserve formatting, finish at EOF even when
// empty, and never register lifecycle followers or consume output appended after completion.
func TestFiniteLogCopiesFinishAtEOF(t *testing.T) {
	t.Parallel()
	one := int64(1)
	for _, testCase := range []struct {
		name     string
		contents string
		options  apiv1.LogOptions
		finished bool
		expected string
	}{
		{name: "empty"},
		{name: "non-follow", contents: "first\nsecond\n", expected: "first\nsecond\n"},
		{name: "completed-follow", contents: "first\nsecond\n", options: apiv1.LogOptions{Follow: true}, finished: true, expected: "first\nsecond\n"},
		{name: "tail", contents: "first\nsecond\n", options: apiv1.LogOptions{Tail: &one}, expected: "second\n"},
		{name: "limit", contents: "first\nsecond\n", options: apiv1.LogOptions{Limit: &one}, expected: "first\n"},
		{name: "skip", contents: "first\nsecond\n", options: apiv1.LogOptions{Skip: &one}, expected: "second\n"},
		{name: "numbering", contents: "1 2026-10-30T12:00:00Z first\n", options: apiv1.LogOptions{LineNumbers: true}, expected: "1 first\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			path := filepath.Join(t.TempDir(), "stdout")
			require.NoError(t, usvc_io.WriteFile(path, []byte(testCase.contents), osutil.PermissionOnlyOwnerReadWrite))
			streamer := &stdIoLogStreamer{lock: &sync.Mutex{}, activeStreams: make(logs.LogStreamMop)}
			defer func() { require.NoError(t, streamer.Dispose()) }()
			executable := &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{Name: "finite", UID: "finite-uid"},
				Status:     apiv1.ExecutableStatus{StdOutFile: path},
			}
			if testCase.finished {
				executable.Status.FinishTimestamp = metav1.NowMicro()
			}
			var output bytes.Buffer
			status, done, streamErr := streamer.StreamLogs(ctx, &output, executable, &testCase.options, logr.Discard())
			require.NoError(t, streamErr)
			require.Equal(t, apiv1.ResourceStreamStatusStreaming, status)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("finite log copy did not finish")
			}
			require.Equal(t, testCase.expected, output.String())
			streamer.lock.Lock()
			followerCount := len(streamer.activeStreams)
			streamer.lock.Unlock()
			require.Zero(t, followerCount)
			appended, appendErr := usvc_io.OpenOrCreateFileForAppending(path, osutil.PermissionOnlyOwnerReadWrite)
			require.NoError(t, appendErr)
			_, writeErr := appended.Write([]byte("after-eof\n"))
			require.NoError(t, writeErr)
			require.NoError(t, appended.Close())
			require.Equal(t, testCase.expected, output.String())
			require.NoError(t, logs.RemoveWithRetry(ctx, path))
		})
	}
}

type finiteCopyStorage struct {
	rest.StandardStorage
	executable *apiv1.Executable
	watcher    *watch.RaceFreeFakeWatcher
}

func (storage *finiteCopyStorage) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	return storage.executable.DeepCopy(), nil
}

func (storage *finiteCopyStorage) Watch(context.Context, *metainternalversion.ListOptions) (watch.Interface, error) {
	return storage.watcher, nil
}

type finiteCopyObserver struct {
	*stdIoLogStreamer
	writing chan struct{}
	started chan (<-chan struct{})
	updated chan struct{}
}

func (observer *finiteCopyObserver) StreamLogs(
	ctx context.Context, dest io.Writer, obj resource.Object, opts *apiv1.LogOptions, log logr.Logger,
) (apiv1.ResourceStreamStatus, <-chan struct{}, error) {
	status, done, streamErr := observer.stdIoLogStreamer.StreamLogs(ctx, &finiteCopyDestination{
		Writer: dest, writing: observer.writing,
	}, obj, opts, log)
	select {
	case observer.started <- done:
	case <-ctx.Done():
	}
	return status, done, streamErr
}

func (observer *finiteCopyObserver) OnResourceUpdated(event apiv1.ResourceWatcherEvent, log logr.Logger) {
	observer.stdIoLogStreamer.OnResourceUpdated(event, log)
	observer.updated <- struct{}{}
}

type finiteCopyDestination struct {
	io.Writer
	writing chan struct{}
	once    sync.Once
}

func (destination *finiteCopyDestination) Write(contents []byte) (int, error) {
	destination.once.Do(func() { close(destination.writing) })
	return destination.Writer.Write(contents)
}

// Verifies that LogStorage releases resource processing during a backpressured finite copy,
// preserves complete delivery, and closes its pipe on cancellation or disposal to unblock the copy.
func TestFiniteLogCopiesReleaseProcessingAndHonorStorageCancellation(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"drain", "cancel", "dispose"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testutil.GetTestContext(t, 0)
			defer cancel()
			requestCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			path := filepath.Join(t.TempDir(), "stdout")
			const expected = "finite-output\n"
			require.NoError(t, usvc_io.WriteFile(path, []byte(expected), osutil.PermissionOnlyOwnerReadWrite))
			parent := &finiteCopyStorage{
				executable: &apiv1.Executable{
					ObjectMeta: metav1.ObjectMeta{Name: "finite", UID: "finite-uid"},
					Status:     apiv1.ExecutableStatus{StdOutFile: path},
				},
				watcher: watch.NewRaceFreeFake(),
			}
			observer := &finiteCopyObserver{
				stdIoLogStreamer: &stdIoLogStreamer{lock: &sync.Mutex{}, activeStreams: make(logs.LogStreamMop)},
				writing:          make(chan struct{}), started: make(chan (<-chan struct{}), 1), updated: make(chan struct{}, 2),
			}
			defer func() { require.NoError(t, observer.Dispose()) }()
			storage, storageErr := apiv1.NewLogStorage(parent, observer)
			require.NoError(t, storageErr)
			defer storage.Destroy()
			response, getErr := storage.Get(requestCtx, parent.executable.Name, &apiv1.LogOptions{})
			require.NoError(t, getErr)
			reader, _, _, inputErr := response.(*apiv1.LogStreamer).InputStream(requestCtx, "", "")
			require.NoError(t, inputErr)
			defer reader.Close()
			parent.watcher.Add(parent.executable.DeepCopy())
			var done <-chan struct{}
			select {
			case done = <-observer.started:
			case <-ctx.Done():
				t.Fatal("stream setup waited for the consumer to read")
			}
			select {
			case <-observer.writing:
			case <-ctx.Done():
				t.Fatal("finite copy did not attempt delivery")
			}
			select {
			case <-done:
				t.Fatal("finite copy finished before the consumer read any output")
			default:
			}
			select {
			case <-observer.updated:
			case <-ctx.Done():
				t.Fatal("initial lifecycle update was not delivered")
			}
			parent.watcher.Modify(parent.executable.DeepCopy())
			select {
			case <-observer.updated:
			case <-ctx.Done():
				t.Fatal("finite copy held the resource processing lock")
			}
			switch action {
			case "drain":
				output, readErr := io.ReadAll(reader)
				require.NoError(t, readErr)
				require.Equal(t, expected, string(output))
			case "cancel":
				cancelRequest()
			case "dispose":
				storage.Destroy()
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("finite copy remained blocked on the response pipe")
			}
			require.NoError(t, logs.RemoveWithRetry(ctx, path))
		})
	}
}
