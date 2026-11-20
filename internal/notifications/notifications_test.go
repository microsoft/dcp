/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package notifications

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/microsoft/dcp/internal/notifications/proto"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/grpcutil"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/testutil"
)

const (
	defaultNotificationsTestTimeout = 20 * time.Second
)

// Verifies that a few notifications can be sent and received.
func TestNotificationSendReceive(t *testing.T) {
	sourceLogSink := testutil.NewMockLoggerSink()
	sourceLog := logr.New(sourceLogSink)

	receiverLogSink := testutil.NewMockLoggerSink()
	receiverLog := logr.New(receiverLogSink)

	ctx, cancel := testutil.GetTestContext(t, defaultNotificationsTestTimeout)
	defer cancel()

	nsi, nsErr := NewNotificationSource(ctx, testutil.TestTempDir(), "test-notification-socket-", sourceLog)
	require.NoError(t, nsErr)
	require.NotNil(t, nsi)
	usns := nsi.(*unixSocketNotificationSource)
	socketPath := nsi.SocketPath()

	const numNotifications = 10
	notes := make(chan Notification, numNotifications)
	callback := func(n Notification) {
		notes <- n
	}

	sub, rcvErr := NewNotificationSubscription(ctx, socketPath, receiverLog, callback)
	require.NoError(t, rcvErr)
	require.NotNil(t, sub)

	swait := usns.clientConnected.Wait()
	select {
	case <-swait.Chan:
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification source to receive a connection")
	}

	for range numNotifications {
		err := usns.NotifySubscribers(&CleanupStartedNotification{})
		require.NoError(t, err)
	}

	// Wait for and verify the notification was received
	for range numNotifications {
		select {
		case note := <-notes:
			require.Equal(t, NotificationKindCleanupStarted, note.Kind(), "Received notification kind does not match expected kind")
		case <-ctx.Done():
			t.Fatal("Timed out waiting for notification")
		}
	}

	// Verify no errors were logged
	sourceLogSink.AssertNotCalled(t, "Error")
	receiverLogSink.AssertNotCalled(t, "Error")
}

// Verifies that multiple receivers can connect and receive notifications.
// When receiver is disconnected, it should not receive any new notifications.
func TestNotificationMultipleReceivers(t *testing.T) {
	t.Parallel()
	testLog := testutil.NewLogForTesting(t.Name())
	ctx, cancel := testutil.GetTestContext(t, defaultNotificationsTestTimeout)
	defer cancel()

	ns, err := NewNotificationSource(ctx, testutil.TestTempDir(), "test-notification-socket-", testLog)
	require.NoError(t, err)
	require.NotNil(t, ns)
	usns := ns.(*unixSocketNotificationSource)
	socketPath := ns.SocketPath()

	// Start with two receivers
	r1Ctx, r1CtxCancel := context.WithCancel(ctx)
	defer r1CtxCancel()
	r1NoteEvt := concurrency.NewAutoResetEvent(false)
	sub1, sub1Err := NewNotificationSubscription(r1Ctx, socketPath, testLog, func(n Notification) { r1NoteEvt.Set() })
	require.NoError(t, sub1Err)
	r1 := sub1.(*notificationReceiver)

	r2Ctx, r2CtxCancel := context.WithCancel(ctx)
	defer r2CtxCancel()
	r2NoteEvt := concurrency.NewAutoResetEvent(false)
	_, sub2Err := NewNotificationSubscription(r2Ctx, socketPath, testLog, func(n Notification) { r2NoteEvt.Set() })
	require.NoError(t, sub2Err)

	// Wait for the receivers to connect
	for range 2 {
		swait := usns.clientConnected.Wait()
		select {
		case <-swait.Chan:
			// Proceed
		case <-ctx.Done():
			t.Fatal("Timed out waiting for notification receivers to connect")
		}
	}

	notifyErr := usns.NotifySubscribers(&CleanupStartedNotification{})
	require.NoError(t, notifyErr)

	// Wait for and verify the notification was received by both receivers
	select {
	case <-r1NoteEvt.Wait():
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification to be received by receiver 1")
	}
	select {
	case <-r2NoteEvt.Wait():
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification to be received by receiver 2")
	}

	// Disconnect receiver 1
	<-r1.connChanged.Wait() // Consume the connection change event (from initial connection) before disconnecting the receiver
	r1CtxCancel()
	<-r1.connChanged.Wait()
	require.True(t, r1.connChanged.Frozen())

	// Verify receiver 2 can still receive notifications
	notifyErr = usns.NotifySubscribers(&CleanupStartedNotification{})
	require.NoError(t, notifyErr)

	select {
	case <-r2NoteEvt.Wait():
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification to be received by receiver 2 after receiver 1 disconnected")
	}

	// Create and connect a new receiver
	r3Ctx, r3CtxCancel := context.WithCancel(ctx)
	defer r3CtxCancel()
	r3NoteEvt := concurrency.NewAutoResetEvent(false)
	_, r3Err := NewNotificationSubscription(r3Ctx, socketPath, testLog, func(n Notification) { r3NoteEvt.Set() })
	require.NoError(t, r3Err)

	swait := usns.clientConnected.Wait()
	select {
	case <-swait.Chan:
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification receiver 3 to connect")
	}

	// Verify receivers 2 and 3 can receive notifications
	notifyErr = usns.NotifySubscribers(&CleanupStartedNotification{})
	require.NoError(t, notifyErr)

	select {
	case <-r2NoteEvt.Wait():
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification to be received by receiver 2 after receiver 1 disconnected")
	}
	select {
	case <-r3NoteEvt.Wait():
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification to be received by receiver 3 after receiver 1 disconnected")
	}
}

// streamFailingNotificationServer sends one notification at the start of every stream
// and fails the first stream with an unexpected gRPC error.
type streamFailingNotificationServer struct {
	proto.UnimplementedNotificationsServer
	subscribeCount atomic.Int32
}

func (s *streamFailingNotificationServer) Subscribe(_ *emptypb.Empty, stream proto.Notifications_SubscribeServer) error {
	note := &proto.NotificationData{Ntype: grpcutil.EnumVal(proto.NotificationType_NTYPE_CLEANUP_STARTED)}
	if sendErr := stream.Send(note); sendErr != nil {
		return sendErr
	}

	if s.subscribeCount.Add(1) == 1 {
		return status.Error(codes.Internal, "synthetic stream failure")
	}

	<-stream.Context().Done()
	return nil
}

// Verifies that NewNotificationSubscription recovers after the notification source fails a stream
// with an error that does not indicate the stream was closed by either side.
func TestNotificationSubscriptionRecoversAfterStreamFailure(t *testing.T) {
	t.Parallel()
	testLog := testutil.NewLogForTesting(t.Name())
	ctx, cancel := testutil.GetTestContext(t, defaultNotificationsTestTimeout)
	defer cancel()

	listener, listenErr := osutil.CreateRandomUnixSocketListener(testutil.TestTempDir(), "test-notification-socket-")
	require.NoError(t, listenErr)

	server := grpc.NewServer()
	proto.RegisterNotificationsServer(server, &streamFailingNotificationServer{})
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Stop()

	received := make(chan Notification, 2)
	sub, subErr := NewNotificationSubscription(ctx, listener.SocketPath(), testLog, func(n Notification) {
		received <- n
	})
	require.NoError(t, subErr)
	require.NotNil(t, sub)

	for receivedCount := range 2 {
		select {
		case note := <-received:
			require.Equal(t, NotificationKindCleanupStarted, note.Kind(), "Received notification kind does not match expected kind")
		case <-ctx.Done():
			t.Fatalf("Timed out waiting for notification %d, subscription did not recover from stream failure", receivedCount+1)
		}
	}
}

// failingSendNotificationStream is a notification subscription stream whose Send always fails.
type failingSendNotificationStream struct {
	grpc.ServerStream
	sendCount atomic.Int32
	sendErr   error
}

func (s *failingSendNotificationStream) Send(*proto.NotificationData) error {
	s.sendCount.Add(1)
	return s.sendErr
}

// Verifies that the notification source ends and removes a subscription when sending a notification fails
// with an unexpected error or with a normal stream termination, instead of retrying the failed stream.
func TestNotificationSourceEndsSubscriptionWhenSendFails(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		sendErr error
	}{
		{
			name:    "UnexpectedError",
			sendErr: status.Error(codes.Internal, "synthetic send failure"),
		},
		{
			name:    "StreamDoneError",
			sendErr: io.EOF,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testLog := testutil.NewLogForTesting(t.Name())
			ctx, cancel := testutil.GetTestContext(t, defaultNotificationsTestTimeout)
			defer cancel()

			ns, nsErr := NewNotificationSource(ctx, testutil.TestTempDir(), "test-notification-socket-", testLog)
			require.NoError(t, nsErr)
			require.NotNil(t, ns)
			usns := ns.(*unixSocketNotificationSource)

			stream := &failingSendNotificationStream{sendErr: tc.sendErr}
			handlerResult := make(chan error, 1)
			go func() {
				handlerResult <- usns.Subscribe(&emptypb.Empty{}, stream)
			}()

			swait := usns.clientConnected.Wait()
			select {
			case <-swait.Chan:
				// Proceed
			case <-ctx.Done():
				t.Fatal("Timed out waiting for notification source to register the subscription")
			}

			require.NoError(t, usns.NotifySubscribers(&CleanupStartedNotification{}))

			select {
			case handlerErr := <-handlerResult:
				require.NoError(t, handlerErr)
			case <-ctx.Done():
				t.Fatal("Timed out waiting for subscription handler to end after a failed notification send")
			}

			usns.lock.Lock()
			remainingSubscriptions := len(usns.subscriptions)
			usns.lock.Unlock()
			require.Zero(t, remainingSubscriptions, "Subscription with a failed notification stream was not removed")

			// Verify the handler does not keep sending on the failed stream.
			require.NoError(t, usns.NotifySubscribers(&CleanupStartedNotification{}))
			require.Equal(t, int32(1), stream.sendCount.Load(), "Subscription handler kept sending on the failed notification stream")
		})
	}
}
