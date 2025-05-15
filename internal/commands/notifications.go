package commands

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"

	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

// Implements a cross-process notification mechanism that lets processes send and receive notifications via Unix domain sockets.

const (
	NotificationSocketPathFlagName = "notification-socket"
	maxNotificationPayloadSize     = 2 * 1024 // 2 KB
	notificationOpTimeout          = 3 * time.Second
)

var (
	notificationSocketPath string
)

func AddNotificationSocketFlag(flags *pflag.FlagSet) {
	flags.StringVar(&notificationSocketPath, NotificationSocketPathFlagName, "", "Specifies the path to the notification socket. This is used to send and receive notifications between processes.")
	_ = flags.MarkHidden(NotificationSocketPathFlagName)
}

func GetNotificationSocketPath() string {
	return notificationSocketPath
}

type NotificationType string

const (
	NotificationTypeCleanupStarted NotificationType = "cleanup-started"
)

type Notification struct {
	// The notification type. This is used to identify the type of notification being sent.
	Type NotificationType `json:"notification_type"`

	// Additional data can be added by embedding Notification struct into another (discriminated union pattern).
}

// NotificationSource creates a Unix domain socket and listens for incoming connections.
// It allows other processes to connect to the socket and receive notifications.
type NotificationSource struct {
	lifetimeCtx      context.Context
	log              logr.Logger
	SocketPath       string
	listener         *net.UnixListener
	connections      *syncmap.Map[uint32, *net.UnixConn]
	stoppedAccepting chan struct{}

	// Mostly used for testing, to signal when a client is connected.
	clientConnected *concurrency.Semaphore
}

func NewNotificationSource(lifetimeCtx context.Context, socketPath string, log logr.Logger) (*NotificationSource, error) {
	listener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if listenErr != nil {
		return nil, fmt.Errorf("could not create notification socket: %w", listenErr)
	}

	ns := &NotificationSource{
		lifetimeCtx:      lifetimeCtx,
		log:              log,
		SocketPath:       socketPath,
		listener:         listener,
		connections:      &syncmap.Map[uint32, *net.UnixConn]{},
		stoppedAccepting: make(chan struct{}),
		clientConnected:  concurrency.NewSemaphore(),
	}
	context.AfterFunc(lifetimeCtx, func() {
		listener.Close() // This also removes the socket file.
		ns.closeConnections()
	})

	go ns.acceptConnections()

	return ns, nil
}

func (ns *NotificationSource) closeConnections() {
	<-ns.stoppedAccepting

	ns.connections.Range(func(connID uint32, conn *net.UnixConn) bool {
		_ = conn.Close()
		return true
	})
}

func (ns *NotificationSource) NotifySubscribers(notification Notification) error {
	nData, jsonErr := json.Marshal(notification)
	if jsonErr != nil {
		return fmt.Errorf("could not marshal notification: %w", jsonErr)
	}
	if len(nData) > maxNotificationPayloadSize {
		return fmt.Errorf("notification payload is too large: %d bytes", len(nData))
	}

	var nDataSize uint32 = uint32(len(nData))
	var dataSizeBytes [4]byte
	binary.BigEndian.PutUint32(dataSizeBytes[:], nDataSize)

	// Write the notification data to all connected subscribers.
	var writeErrors error

	ns.connections.Range(func(connID uint32, conn *net.UnixConn) bool {
		if ns.lifetimeCtx.Err() != nil {
			writeErrors = errors.Join(writeErrors, ns.lifetimeCtx.Err())
			return false // We are shutting down, stop iterating
		}

		// Write notification payload size first, then the notification data itself, then try to get the acknowledgment.
		// This is a simple protocol, similar to what IP protocol family often uses,
		// but will do for our use case. If we ever need something more complex, consider gRPC.

		// On any I/O error, just close the connection, it is much easier for the client to handle
		// than to try to recover from a partial write.
		// Don't even report the error as it is hard to predict what error will be returned depending on the OS/OS version.
		// E.g. on MacOS the write might return a "broken pipe" error even if the client properly closes the connection.

		deadlineErr := conn.SetDeadline(time.Now().Add(notificationOpTimeout))
		if deadlineErr != nil {
			if !errors.Is(deadlineErr, net.ErrClosed) {
				_ = conn.Close()
			}
			ns.connections.Delete(connID)
			return true
		}

		for _, buf := range [][]byte{dataSizeBytes[:], nData} {
			if ns.lifetimeCtx.Err() != nil {
				break
			}

			_, writeErr := conn.Write(buf)
			if writeErr != nil {
				if !errors.Is(writeErr, net.ErrClosed) {
					_ = conn.Close()
				}
				ns.connections.Delete(connID)
				return true
			}
		}

		var ackBuf [3]byte
		_, readErr := io.ReadFull(conn, ackBuf[:])
		if readErr != nil {
			if !errors.Is(readErr, net.ErrClosed) {
				_ = conn.Close()
			}
			ns.connections.Delete(connID)
			return true
		}
		if !bytes.Equal(ackBuf[:], []byte("ACK")) {
			writeErrors = errors.Join(writeErrors, fmt.Errorf("invalid acknowledgment from connection %d: %s", connID, string(ackBuf[:])))
			ns.connections.Delete(connID)
			return true
		}

		return true
	})

	return writeErrors
}

func (ns *NotificationSource) acceptConnections() {
	var nextConnID uint32 = 1
	defer close(ns.stoppedAccepting)

	for {
		if ns.lifetimeCtx.Err() != nil {
			return // We are closed/shutting down.
		}

		conn, err := ns.listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // Listener was closed, nothing more to do
			}
			ns.log.Error(err, "error accepting connection on notification socket")
			continue
		}

		ns.connections.Store(nextConnID, conn)
		nextConnID++
		ns.clientConnected.Signal()
	}
}

// NotificationReceiver connects to a notification socket and receives notifications.
type NotificationReceiver struct {
	lifetimeCtx context.Context
	log         logr.Logger
	socketPath  string
	callback    func(Notification)

	// Set when the receiver is connChanged to the socket. Frozen when the lifetime context expires
	// and the receiver no longer receives any notifications. This is mostly used for testing.
	connChanged *concurrency.AutoResetEvent
}

// NewNotificationReceiver creates a new notification receiver.
func NewNotificationReceiver(
	lifetimeCtx context.Context,
	socketPath string,
	log logr.Logger,
	callback func(Notification),
) (*NotificationReceiver, error) {
	if callback == nil {
		return nil, fmt.Errorf("callback cannot be nil")
	}

	nr := &NotificationReceiver{
		lifetimeCtx: lifetimeCtx,
		log:         log,
		socketPath:  socketPath,
		callback:    callback,
		connChanged: concurrency.NewAutoResetEvent(false),
	}

	// Start the receiver goroutine
	go nr.receiveLoop()

	return nr, nil
}

// receiveLoop is the main loop that handles connecting to the socket and receiving notifications.
func (nr *NotificationReceiver) receiveLoop() {
	defer nr.connChanged.SetAndFreeze()

	for {
		if nr.lifetimeCtx.Err() != nil {
			return
		}

		conn, connectErr := resiliency.RetryGetExponential(nr.lifetimeCtx, func() (*net.UnixConn, error) {
			return net.DialUnix("unix", nil, &net.UnixAddr{Name: nr.socketPath, Net: "unix"})
		})
		if connectErr != nil {
			if nr.lifetimeCtx.Err() == nil {
				nr.log.Error(connectErr, "Failed to connect to notification socket")
			}
			return // Give up
		}

		if nr.lifetimeCtx.Err() != nil {
			_ = conn.Close()
			return // Shutting down
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

func (nr *NotificationReceiver) receiveNotifications(conn *net.UnixConn) {
	var dataSizeBytes [4]byte
	var dataSize uint32

	defer func() {
		_ = conn.Close()
	}()

	notClosedOrEOF := func(err error) bool {
		return !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF)
	}

	for {
		if nr.lifetimeCtx.Err() != nil {
			return
		}

		err := conn.SetDeadline(time.Now().Add(notificationOpTimeout))
		if err != nil {
			if notClosedOrEOF(err) {
				nr.log.Error(err, "Failed to set read deadline")
			}
			return
		}

		n, err := io.ReadFull(conn, dataSizeBytes[:])
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue // Expected, retry read
			}
			if notClosedOrEOF(err) {
				nr.log.Error(err, "Error reading notification size", "BytesRead", n)
			}
			return
		}
		if nr.lifetimeCtx.Err() != nil {
			return
		}

		dataSize = binary.BigEndian.Uint32(dataSizeBytes[:])
		if dataSize > maxNotificationPayloadSize {
			nr.log.Error(fmt.Errorf("notification payload size too large"), "Size", dataSize)
			return
		}

		err = conn.SetDeadline(time.Now().Add(notificationOpTimeout))
		if err != nil {
			if notClosedOrEOF(err) {
				nr.log.Error(err, "Failed to set read deadline")
			}
			return
		}

		payloadBuf := make([]byte, dataSize)
		n, err = io.ReadFull(conn, payloadBuf)
		if err != nil {
			if notClosedOrEOF(err) {
				nr.log.Error(err, "Error reading notification payload", "BytesRead", n)
			}
			return
		}

		var notification Notification
		err = json.Unmarshal(payloadBuf, &notification)
		if err != nil {
			nr.log.Error(err, "Error unmarshalling notification", "payload", string(payloadBuf))
			return
		}

		nr.callback(notification)

		ack := []byte("ACK")
		_, err = conn.Write(ack)
		if err != nil {
			if notClosedOrEOF(err) {
				nr.log.Error(err, "Error writing acknowledgment")
			}
			return
		}
	}
}
