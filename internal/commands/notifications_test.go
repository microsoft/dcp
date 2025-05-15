package commands

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/testutil"
)

const (
	defaultNotificationsTestTimeout = 20 * time.Second
)

// Verifies that a few notifications can be sent and received.
func TestNotificationSendReceive(t *testing.T) {
	sourceLogSink := &testutil.MockLoggerSink{}
	sourceLogSink.On("Init", mock.AnythingOfType("logr.RuntimeInfo")).Return()
	sourceLog := logr.New(sourceLogSink)

	receiverLogSink := &testutil.MockLoggerSink{}
	receiverLogSink.On("Init", mock.AnythingOfType("logr.RuntimeInfo")).Return()
	receiverLog := logr.New(receiverLogSink)

	ctx, cancel := testutil.GetTestContext(t, defaultNotificationsTestTimeout)
	defer cancel()

	suffix, suffixErr := randdata.MakeRandomString(8)
	require.NoError(t, suffixErr)
	socketPath := filepath.Join(testutil.TestTempDir(), "test-notification-socket-"+string(suffix))
	ns, nsErr := NewNotificationSource(ctx, socketPath, sourceLog)
	require.NoError(t, nsErr)
	require.NotNil(t, ns)

	const numNotifications = 10
	notes := make(chan Notification, numNotifications)
	callback := func(n Notification) {
		notes <- n
	}

	nr, rcvErr := NewNotificationReceiver(ctx, socketPath, receiverLog, callback)
	require.NoError(t, rcvErr)
	require.NotNil(t, nr)

	swait := ns.clientConnected.Wait()
	select {
	case <-swait.Chan:
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification source to receive a connection")
	}

	testNotification := Notification{
		Type: NotificationTypeCleanupStarted,
	}

	for range numNotifications {
		err := ns.NotifySubscribers(testNotification)
		require.NoError(t, err)
	}

	// Wait for and verify the notification was received
	for range numNotifications {
		select {
		case note := <-notes:
			require.Equal(t, NotificationTypeCleanupStarted, note.Type)
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

	suffix, suffixErr := randdata.MakeRandomString(8)
	require.NoError(t, suffixErr)
	socketPath := filepath.Join(testutil.TestTempDir(), "test-notification-socket-"+string(suffix))
	ns, err := NewNotificationSource(ctx, socketPath, testLog)
	require.NoError(t, err)
	require.NotNil(t, ns)

	// Start with two receivers
	r1Ctx, r1CtxCancel := context.WithCancel(ctx)
	defer r1CtxCancel()
	r1NoteEvt := concurrency.NewAutoResetEvent(false)
	r1, r1Err := NewNotificationReceiver(r1Ctx, socketPath, testLog, func(n Notification) { r1NoteEvt.Set() })
	require.NoError(t, r1Err)

	r2Ctx, r2CtxCancel := context.WithCancel(ctx)
	defer r2CtxCancel()
	r2NoteEvt := concurrency.NewAutoResetEvent(false)
	_, r2Err := NewNotificationReceiver(r2Ctx, socketPath, testLog, func(n Notification) { r2NoteEvt.Set() })
	require.NoError(t, r2Err)

	// Wait for the receivers to connect
	for range 2 {
		swait := ns.clientConnected.Wait()
		select {
		case <-swait.Chan:
			// Proceed
		case <-ctx.Done():
			t.Fatal("Timed out waiting for notification receivers to connect")
		}
	}

	notifyErr := ns.NotifySubscribers(Notification{Type: NotificationTypeCleanupStarted})
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
	notifyErr = ns.NotifySubscribers(Notification{Type: NotificationTypeCleanupStarted})
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
	_, r3Err := NewNotificationReceiver(r3Ctx, socketPath, testLog, func(n Notification) { r3NoteEvt.Set() })
	require.NoError(t, r3Err)

	swait := ns.clientConnected.Wait()
	select {
	case <-swait.Chan:
		// Proceed
	case <-ctx.Done():
		t.Fatal("Timed out waiting for notification receiver 3 to connect")
	}

	// Verify receivers 2 and 3 can receive notifications
	notifyErr = ns.NotifySubscribers(Notification{Type: NotificationTypeCleanupStarted})
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
