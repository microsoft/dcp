// Copyright (c) Microsoft Corporation. All rights reserved.

package notifications

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/microsoft/dcp/internal/notifications/proto"
	"github.com/microsoft/dcp/pkg/concurrency"
	"github.com/microsoft/dcp/pkg/grpcutil"
)

type notificationReceiver struct {
	lifetimeCtx context.Context
	log         logr.Logger
	socketPath  string
	callback    func(Notification)

	// Set when the receiver is connected to the socket. Frozen when the lifetime context expires
	// and the receiver no longer receives any notifications. This is mostly used for testing.
	connChanged *concurrency.AutoResetEvent
}

func (nr *notificationReceiver) Active() bool {
	return !nr.connChanged.Frozen()
}

// receiveLoop is the main loop that handles connecting to the socket and receiving notifications.
func (nr *notificationReceiver) receiveLoop() {
	defer nr.connChanged.SetAndFreeze()

	for {
		if nr.lifetimeCtx.Err() != nil {
			return
		}

		conn, connErr := grpc.NewClient("unix:"+nr.socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if connErr != nil {
			if nr.lifetimeCtx.Err() == nil {
				nr.log.Error(connErr, "Failed to connect to notification service", "SocketPath", nr.socketPath)
			}
			return // Give up
		}

		nr.connChanged.Set()
		nr.receiveNotifications(conn)

		// Small delay before retrying the whole connect+receive process
		select {
		case <-nr.lifetimeCtx.Done():
			return
		case <-time.After(2 * time.Second):
			// Try again
		}
	}
}

func (nr *notificationReceiver) receiveNotifications(conn *grpc.ClientConn) {
	defer func() {
		_ = conn.Close()
	}()

	client := proto.NewNotificationsClient(conn)
	stream, streamErr := client.Subscribe(nr.lifetimeCtx, &emptypb.Empty{})
	if streamErr != nil {
		nr.log.Error(streamErr, "Failed to subscribe to notifications")
		return
	}

	b := grpcutil.StreamRetryBackoff()

	for {
		select {
		case <-nr.lifetimeCtx.Done():
			return

		default:
			nd, recvErr := stream.Recv()

			if grpcutil.IsStreamDoneErr(recvErr) {
				nr.log.V(1).Info("Notification stream is done")
				return
			}

			if recvErr != nil {
				nr.log.Error(recvErr, "Failed to receive notification from stream")
				time.Sleep(b.NextBackOff())
				continue
			}

			n, convErr := asNotification(nd)
			if convErr != nil {
				nr.log.Error(convErr, "Received notification is not valid")
				time.Sleep(b.NextBackOff())
				continue
			}

			b.Reset()
			nr.callback(n)
		}
	}
}

var _ NotificationSubscription = (*notificationReceiver)(nil)
