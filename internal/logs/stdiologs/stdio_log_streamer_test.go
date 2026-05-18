/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package stdiologs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/internal/logs"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/testutil"
)

const stdioLogStreamerTestTimeout = 20 * time.Second

func TestOnResourceUpdatedDelaysDeletingResourceStreamStop(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		eventType      watch.EventType
		markAsDeleting bool
	}{
		{
			name:           "modified with deletion timestamp",
			eventType:      watch.Modified,
			markAsDeleting: true,
		},
		{
			name:      "deleted",
			eventType: watch.Deleted,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := testutil.GetTestContext(t, stdioLogStreamerTestTimeout)
			defer cancel()

			streamer := &stdIoLogStreamer{
				lock:          &sync.Mutex{},
				activeStreams: make(logs.LogStreamMop),
			}
			log := testutil.NewLogForTesting("stdio-log-streamer-delete")
			followWriter := newEOFFollowWriter(ctx)
			resourceUID := types.UID("delete-stream-test")
			streamer.activeStreams[resourceUID] = map[logs.LogStreamID]*usvc_io.FollowWriter{
				1: followWriter,
			}
			exe := &apiv1.Executable{
				ObjectMeta: metav1.ObjectMeta{
					Name: "delete-stream-test",
					UID:  resourceUID,
				},
			}

			if testCase.markAsDeleting {
				now := metav1.Now()
				exe.DeletionTimestamp = &now
			}
			streamer.OnResourceUpdated(apiv1.ResourceWatcherEvent{
				Type:   testCase.eventType,
				Object: exe,
			}, log)

			assertStreamNotDoneImmediately(t, ctx, followWriter.Done())
			assertStreamDone(t, ctx, followWriter.Done())
		})
	}
}

func TestDisposeCancelsStreamsImmediately(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, stdioLogStreamerTestTimeout)
	defer cancel()

	streamer := &stdIoLogStreamer{
		lock:          &sync.Mutex{},
		activeStreams: make(logs.LogStreamMop),
	}
	followWriter := newBlockingFollowWriter(ctx)
	streamer.activeStreams[types.UID("dispose-stream-test")] = map[logs.LogStreamID]*usvc_io.FollowWriter{
		1: followWriter,
	}

	require.NoError(t, streamer.Dispose())
	assertDoneBeforeFollowDelay(t, ctx, followWriter.Done())
}

func newBlockingFollowWriter(ctx context.Context) *usvc_io.FollowWriter {
	reader, _ := usvc_io.NewBufferedPipe()
	return usvc_io.NewFollowWriter(ctx, reader, testutil.NewBufferWriter(), usvc_io.WithCloseSourceOnCancel())
}

func newEOFFollowWriter(ctx context.Context) *usvc_io.FollowWriter {
	return usvc_io.NewFollowWriter(ctx, testutil.NewThreadSafeBuffer(), testutil.NewBufferWriter())
}

func assertDoneBeforeFollowDelay(t *testing.T, ctx context.Context, done <-chan struct{}) {
	t.Helper()

	timer := time.NewTimer(logs.FollowStreamCancellationDelay / 2)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		t.Fatal("log stream was not canceled before follow cancellation delay")
	case <-ctx.Done():
		t.Fatal("test timed out before log stream was canceled")
	}
}

func assertStreamNotDoneImmediately(t *testing.T, ctx context.Context, done <-chan struct{}) {
	t.Helper()

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-done:
		t.Fatal("log stream was canceled immediately")
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("test timed out while checking that log stream was not canceled immediately")
	}
}

func assertStreamDone(t *testing.T, ctx context.Context, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("test timed out before log stream was canceled")
	}
}
