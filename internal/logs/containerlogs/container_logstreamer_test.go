/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package containerlogs

import (
	"context"
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

const containerLogStreamerTestTimeout = 20 * time.Second

func TestOnResourceUpdatedCancelsDeletingContainerStreamsImmediately(t *testing.T) {
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

			ctx, cancel := testutil.GetTestContext(t, containerLogStreamerTestTimeout)
			defer cancel()

			log := testutil.NewLogForTesting("container-log-streamer-delete")
			streamer := NewLogStreamer(log)
			containerUID := types.UID("delete-container-stream-test")
			followWriters := addBlockingContainerFollowWriters(ctx, streamer, containerUID)
			ctr := &apiv1.Container{
				ObjectMeta: metav1.ObjectMeta{
					Name: "delete-container-stream-test",
					UID:  containerUID,
				},
				Status: apiv1.ContainerStatus{
					State: apiv1.ContainerStateRunning,
				},
			}
			if testCase.markAsDeleting {
				now := metav1.Now()
				ctr.DeletionTimestamp = &now
			}

			streamer.OnResourceUpdated(apiv1.ResourceWatcherEvent{
				Type:   testCase.eventType,
				Object: ctr,
			}, log)

			for _, followWriter := range followWriters {
				assertContainerStreamDoneBeforeFollowDelay(t, ctx, followWriter.Done())
			}
		})
	}
}

func TestDisposeCancelsContainerStreamsImmediately(t *testing.T) {
	t.Parallel()

	ctx, cancel := testutil.GetTestContext(t, containerLogStreamerTestTimeout)
	defer cancel()

	log := testutil.NewLogForTesting("container-log-streamer-dispose")
	streamer := NewLogStreamer(log)
	followWriters := addBlockingContainerFollowWriters(ctx, streamer, types.UID("dispose-container-stream-test"))

	require.NoError(t, streamer.Dispose())
	for _, followWriter := range followWriters {
		assertContainerStreamDoneBeforeFollowDelay(t, ctx, followWriter.Done())
	}
}

func addBlockingContainerFollowWriters(ctx context.Context, streamer *containerLogStreamer, containerUID types.UID) []*usvc_io.FollowWriter {
	followWriters := []*usvc_io.FollowWriter{
		newBlockingContainerFollowWriter(ctx),
		newBlockingContainerFollowWriter(ctx),
		newBlockingContainerFollowWriter(ctx),
	}
	streamer.startupLogStreams[containerUID] = map[logs.LogStreamID]*usvc_io.FollowWriter{
		1: followWriters[0],
	}
	streamer.stdioLogStreams[containerUID] = map[logs.LogStreamID]*usvc_io.FollowWriter{
		2: followWriters[1],
	}
	streamer.systemLogStreams[containerUID] = map[logs.LogStreamID]*usvc_io.FollowWriter{
		3: followWriters[2],
	}
	return followWriters
}

func newBlockingContainerFollowWriter(ctx context.Context) *usvc_io.FollowWriter {
	reader, _ := usvc_io.NewBufferedPipe()
	return usvc_io.NewFollowWriter(ctx, reader, testutil.NewBufferWriter())
}

func assertContainerStreamDoneBeforeFollowDelay(t *testing.T, ctx context.Context, done <-chan struct{}) {
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
