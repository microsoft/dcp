/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package v1

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/watch"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"

	"github.com/microsoft/dcp/pkg/testutil"
)

// Verifies that LogStorage.watchResource does not allocate a resource watcher when parent
// watch setup fails and can establish the watch on a subsequent request.
func TestLogStorageWatchSetupFailureCanBeRetried(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, 0)
	defer cancel()
	setupErr := errors.New("test watch setup failure")
	parentWatcher := watch.NewRaceFreeFake()
	parentStorage := &logStorageWatchSource{watcher: parentWatcher, watchErr: setupErr}
	storage, storageErr := NewLogStorage(parentStorage, logStorageTestStreamer{})
	require.NoError(t, storageErr)
	t.Cleanup(storage.Destroy)

	resourceWatcher, watchErr := storage.watchResource("resource", logr.Discard())
	require.ErrorIs(t, watchErr, setupErr)
	require.Nil(t, resourceWatcher)
	require.Nil(t, storage.watcher)
	require.True(t, storage.resourceWatchers.Empty())

	parentStorage.watchErr = nil
	retryWatcher, retryErr := storage.watchResource("resource", logr.Discard())
	require.NoError(t, retryErr)
	retryWatcher.Stop()
	select {
	case _, open := <-retryWatcher.ResultChan():
		require.False(t, open)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	storage.Destroy()
	require.True(t, parentWatcher.IsStopped())
}

// Verifies that LogStorage.Destroy closes its disposal signal without a parent watcher,
// remains idempotent, and rejects subsequent resource-watch requests.
func TestLogStorageDestroyWithoutParentWatcher(t *testing.T) {
	storage, storageErr := NewLogStorage(&logStorageWatchSource{}, logStorageTestStreamer{})
	require.NoError(t, storageErr)
	t.Cleanup(storage.Destroy)

	storage.Destroy()
	storage.Destroy()
	select {
	case <-storage.disposeCh:
	default:
		t.Fatal("log storage disposal signal was not closed")
	}
	resourceWatcher, watchErr := storage.watchResource("resource", logr.Discard())
	require.ErrorContains(t, watchErr, "disposed")
	require.Nil(t, resourceWatcher)
	require.True(t, storage.resourceWatchers.Empty())
}

type logStorageWatchSource struct {
	registry_rest.StandardStorage
	watcher  watch.Interface
	watchErr error
}

func (source *logStorageWatchSource) Watch(context.Context, *metainternalversion.ListOptions) (watch.Interface, error) {
	if source.watchErr != nil {
		return nil, source.watchErr
	}
	return source.watcher, nil
}

type logStorageTestStreamer struct{}

func (logStorageTestStreamer) Preflight(apiserver_resource.Object, *LogOptions) error {
	return nil
}

func (logStorageTestStreamer) StreamLogs(context.Context, io.Writer, apiserver_resource.Object, *LogOptions, logr.Logger) (ResourceStreamStatus, <-chan struct{}, error) {
	return ResourceStreamStatusDone, nil, nil
}

func (logStorageTestStreamer) OnResourceUpdated(ResourceWatcherEvent, logr.Logger) {}

func (logStorageTestStreamer) Dispose() error {
	return nil
}
