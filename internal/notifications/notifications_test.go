/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package notifications

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/dcp/pkg/concurrency"
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

	socketPath, socketPathErr := PrepareNotificationSocketPath(testutil.TestTempDir(), "test-notification-socket-")
	require.NoError(t, socketPathErr)
	nsi, nsErr := NewNotificationSource(ctx, socketPath, sourceLog)
	require.NoError(t, nsErr)
	require.NotNil(t, nsi)
	usns := nsi.(*unixSocketNotificationSource)

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

	socketPath, socketPathErr := PrepareNotificationSocketPath(testutil.TestTempDir(), "test-notification-socket-")
	require.NoError(t, socketPathErr)
	ns, err := NewNotificationSource(ctx, socketPath, testLog)
	require.NoError(t, err)
	require.NotNil(t, ns)
	usns := ns.(*unixSocketNotificationSource)

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
