/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package logstream

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/microsoft/dcp/pkg/testutil"
)

type storageOptions struct{ LogOptions }

func (opts *storageOptions) DeepCopyObject() runtime.Object {
	copied := *opts
	return &copied
}
func (opts *storageOptions) GetFollow() bool { return opts.Follow }

type unusedStorage struct{ rest.StandardStorage }
type unusedStreamer struct {
	ResourceLogStreamer[*storageOptions]
}

// Verifies that log storage initializes its disposal signal and can be destroyed repeatedly
// without a watcher or any registered client streams.
func TestLogStorageDisposeWithoutWatchers(t *testing.T) {
	storage, storageErr := NewLogStorage(&unusedStorage{}, &unusedStreamer{},
		func() *storageOptions { return &storageOptions{} },
		func(rest.ResourceStreamer) runtime.Object { return &metav1.Status{} },
		schema.GroupVersionKind{Group: "test", Version: "v1", Kind: "LogResponse"})
	require.NoError(t, storageErr)
	storage.Destroy()
	storage.Destroy()
	select {
	case <-storage.disposeCh:
	default:
		require.FailNow(t, "storage did not close its disposal signal")
	}
}

// Verifies that resource watchers deliver valid events and stop idempotently without
// accepting new events or leaving a result channel open.
func TestResourceWatcherStopsDelivery(t *testing.T) {
	ctx, cancel := testutil.GetTestContext(t, time.Minute)
	defer cancel()
	watcher := newResourceWatcher()
	defer watcher.Stop()
	require.True(t, watcher.Queue(ResourceWatcherEvent{Type: watch.Added}))
	select {
	case event, open := <-watcher.ResultChan():
		require.True(t, open)
		require.Equal(t, watch.Added, event.Type)
	case <-ctx.Done():
		require.FailNow(t, "resource event was not delivered")
	}
	watcher.Stop()
	watcher.Stop()
	require.False(t, watcher.Queue(ResourceWatcherEvent{Type: watch.Modified}))
	select {
	case _, open := <-watcher.ResultChan():
		require.False(t, open)
	case <-ctx.Done():
		require.FailNow(t, "resource watcher did not close")
	}
}
