package notifications

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/microsoft/usvc-apiserver/internal/notifications/proto"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
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

	for {
		select {
		case <-nr.lifetimeCtx.Done():
			return

		default:
			nd, recvErr := stream.Recv()

			if recvErr == io.EOF || status.Code(recvErr) == codes.Canceled {
				nr.log.V(1).Info("Notification stream closed by the source")
				return
			}

			if errors.Is(recvErr, context.Canceled) {
				nr.log.V(1).Info("Notification stream context expired, shutting down NotificationReceiver...")
				return
			}

			if recvErr != nil {
				nr.log.Error(recvErr, "Failed to receive notification from stream")
				continue
			}

			n, convErr := asNotification(nd)
			if convErr != nil {
				nr.log.Error(convErr, "Recieved notification is not valid")
				continue
			}

			nr.callback(n)
		}
	}
}

var _ NotificationSubscription = (*notificationReceiver)(nil)
