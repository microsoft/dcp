// Copyright (c) Microsoft Corporation. All rights reserved.

package notifications

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/microsoft/dcp/internal/notifications/proto"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/grpcutil"
)

var (
	nextSubscriptionID uint32 = 0
)

// unixSocketNotificationSource creates a Unix domain socket and listens for incoming connections.
// It allows other processes to connect to the socket and receive notifications.
// Data is exchanged using gRPC messages defined in the proto package.
type unixSocketNotificationSource struct {
	// Need to embed the following to ensure gRPC forward compatibility.
	proto.UnimplementedNotificationsServer

	lifetimeCtx context.Context
	log         logr.Logger
	socketPath  string
	lock        *sync.Mutex

	// The Unix domain socket listener for incoming connections.
	listener *net.UnixListener

	// Subscriptions are just long-lived gRPC calls returning a stream of notifications.
	// Each channel gets an unbounded channel for sending notifications to the client/subscriber.
	subscriptions map[uint32]*concurrency.UnboundedChan[Notification]

	// The context, and context cancellation function for the subscriptions.
	// We want to keep it separate from the lifetime context to allow for graceful closing of the subscriptions.
	subCtx       context.Context
	subCtxCancel context.CancelFunc

	// One-time job representing the disposal of the NotificationSource.
	dispose *concurrency.OneTimeJob[struct{}]

	// Mostly used for testing, to signal when a client is connected.
	clientConnected *concurrency.Semaphore
}

// Handles incoming gRPC Subscribe requests.
func (ns *unixSocketNotificationSource) Subscribe(in *emptypb.Empty, stream proto.Notifications_SubscribeServer) error {
	ns.lock.Lock()
	if ns.dispose.IsDone() {
		ns.lock.Unlock()
		return status.Error(codes.FailedPrecondition, "NotificationSource is disposed")
	}
	sid := atomic.AddUint32(&nextSubscriptionID, 1)
	schan := concurrency.NewUnboundedChan[Notification](ns.subCtx)
	ns.subscriptions[sid] = schan
	ns.lock.Unlock()

	ns.log.V(1).Info("New client connected to notification stream", "SubscriptionID", sid)
	ns.clientConnected.Signal()

	forgetSubscription := func() {
		ns.lock.Lock()
		defer ns.lock.Unlock()
		delete(ns.subscriptions, sid)
	}

	for {
		select {

		case <-ns.subCtx.Done():
			forgetSubscription()
			return nil

		case n := <-schan.Out:
			nd, ndErr := asNotificationData(n)
			if ndErr != nil {
				ns.log.Error(ndErr, "Failed to convert notification to NotificationData", "Notification", n)
				return status.Error(codes.Internal, "failed to convert notification data")
			}

			sendErr := stream.Send(nd)

			if grpcutil.IsStreamDoneErr(sendErr) {
				forgetSubscription()
				return nil
			}

			if sendErr != nil {
				ns.log.Error(sendErr, "Failed to send notification to client", "SubscriptionID", sid, "Notification", n)
				continue // Try again with next notification.
			}
		}
	}
}

// Notifies all subscribers with the given notification.
// Notifications are delivered asynchronously. The caller should not assume that all subscribers
// have received the notification by the time this method returns.
func (ns *unixSocketNotificationSource) NotifySubscribers(n Notification) error {
	if n == nil {
		return fmt.Errorf("nil notification")
	}

	ns.lock.Lock()
	if ns.dispose.IsDone() {
		ns.lock.Unlock()
		return errors.New("NotificationSource is disposed, cannot notify subscribers")
	}
	defer ns.lock.Unlock()
	for _, schan := range ns.subscriptions {
		schan.In <- n
	}
	return nil
}

func (ns *unixSocketNotificationSource) SocketPath() string {
	return ns.socketPath
}

func (ns *unixSocketNotificationSource) disposeOnce() {
	if !ns.dispose.TryTake() {
		return // Already disposed
	}
	defer ns.dispose.Complete(struct{}{})
	ns.subCtxCancel()
	_ = ns.listener.Close() // This also removes the socket file
}

var _ proto.NotificationsServer = (*unixSocketNotificationSource)(nil)
var _ UnixSocketNotificationSource = (*unixSocketNotificationSource)(nil)
