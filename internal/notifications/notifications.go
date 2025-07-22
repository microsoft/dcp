package notifications

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/microsoft/usvc-apiserver/internal/dcp/dcppaths"
	"github.com/microsoft/usvc-apiserver/internal/notifications/proto"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
	"github.com/microsoft/usvc-apiserver/pkg/grpcutil"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
)

type NotificationKind string

const (
	NotificationKindCleanupStarted   NotificationKind = "cleanup-started"
	NotificationKindPerftraceRequest NotificationKind = "perftrace-request"

	NotificationSocketPathFlagName = "notification-socket"
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

type Notification interface {
	Kind() NotificationKind
}

type CleanupStartedNotification struct{}

var _ Notification = (*CleanupStartedNotification)(nil)

func (n *CleanupStartedNotification) Kind() NotificationKind {
	return NotificationKindCleanupStarted
}

type PerftraceRequestNotification struct {
	Duration time.Duration
}

var _ Notification = (*PerftraceRequestNotification)(nil)

func (n *PerftraceRequestNotification) Kind() NotificationKind {
	return NotificationKindPerftraceRequest
}

func asNotificationData(n Notification) (*proto.NotificationData, error) {
	if n == nil {
		return nil, fmt.Errorf("nil notification")
	}

	switch n.Kind() {

	case NotificationKindCleanupStarted:
		return &proto.NotificationData{
			Ntype: grpcutil.EnumVal(proto.NotificationType_NTYPE_CLEANUP_STARTED),
		}, nil

	case NotificationKindPerftraceRequest:
		req := n.(*PerftraceRequestNotification)
		return &proto.NotificationData{
			Ntype: grpcutil.EnumVal(proto.NotificationType_NTYPE_PERFTRACE_REQUEST),
			Data: &proto.NotificationData_PerftraceRequest{
				PerftraceRequest: &proto.PerftraceRequest{
					Duration: durationpb.New(req.Duration),
				},
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown notification kind: %s", n.Kind())

	}
}

func asNotification(nd *proto.NotificationData) (Notification, error) {
	if nd == nil {
		return nil, fmt.Errorf("nil notification data")
	}

	switch nd.GetNtype() {

	case proto.NotificationType_NTYPE_CLEANUP_STARTED:
		return &CleanupStartedNotification{}, nil

	case proto.NotificationType_NTYPE_PERFTRACE_REQUEST:
		ptr := nd.GetPerftraceRequest()
		if ptr == nil {
			return nil, fmt.Errorf("missing perftrace request data")
		}
		d := ptr.GetDuration().AsDuration()
		if d <= 0 {
			return nil, fmt.Errorf("invalid perftrace request duration: %v", d)
		}
		return &PerftraceRequestNotification{
			Duration: d,
		}, nil

	default:
		return nil, fmt.Errorf("unknown notification type: %s", nd.GetNtype().String())
	}
}

// A helper function that ensures the notification socket can be created
// in a folder that is writable only by the current user, and that the path
// is reasonably unique to the calling process.
// If the `rootDir` is empty, it will use the user's cache directory.
func PrepareNotificationSocketPath(rootDir string, socketNamePrefix string) (string, error) {
	if rootDir == "" {
		cacheDir, cacheDirErr := os.UserCacheDir()
		if cacheDirErr != nil {
			return "", fmt.Errorf("failed to get user cache directory when creating a notification socket: %w", cacheDirErr)
		} else {
			rootDir = cacheDir
		}
	}

	socketDir := filepath.Join(rootDir, dcppaths.DcpWorkDir)
	if err := os.MkdirAll(socketDir, osutil.PermissionOnlyOwnerReadWriteTraverse); err != nil {
		return "", fmt.Errorf("failed to create directory for notification socket: %w", err)
	}

	// On Windows the user cache directory always exists and is always private to the user,
	// but on Unix-like systems, we need to ensure the directory is private.
	if !osutil.IsWindows() {
		info, infoErr := os.Stat(socketDir)
		if infoErr != nil {
			return "", fmt.Errorf("failed to check permissions on the notification socket directory: %w", infoErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("notification socket path %s is not a directory", socketDir)
		}
		if info.Mode().Perm() != osutil.PermissionOnlyOwnerReadWriteTraverse {
			return "", fmt.Errorf("notification socket directory %s is not private to the user", socketDir)
		}
	}

	suffix, suffixErr := randdata.MakeRandomString(8)
	if suffixErr != nil {
		return "", fmt.Errorf("failed to create random string for notification socket path suffix: %w", suffixErr)
	}

	socketPath := filepath.Join(socketDir, socketNamePrefix+string(suffix))
	return socketPath, nil
}

// NotificationSubscription represents a subscription to notifications.
type NotificationSubscription interface {
	Active() bool // True if the subscription is active and receiving notifications.
}

// NewNotificationSubscription creates a new notification subscription that will result
// in the callback being called whenever a notification is received from the socket.
func NewNotificationSubscription(
	lifetimeCtx context.Context,
	socketPath string,
	log logr.Logger,
	callback func(Notification),
) (NotificationSubscription, error) {
	if callback == nil {
		return nil, fmt.Errorf("callback cannot be nil")
	}

	nr := &notificationReceiver{
		lifetimeCtx: lifetimeCtx,
		log:         log,
		socketPath:  socketPath,
		callback:    callback,
		connChanged: concurrency.NewAutoResetEvent(false),
	}

	go nr.receiveLoop()

	return nr, nil
}

// NotificationSource is a thing capable of sending notifications to subscribers.
type NotificationSource interface {
	NotifySubscribers(n Notification) error
}

type NotifySubscribersFunc func(n Notification) error

func (f NotifySubscribersFunc) NotifySubscribers(n Notification) error {
	return f(n)
}

type UnixSocketNotificationSource interface {
	NotificationSource
	SocketPath() string
}

func NewNotificationSource(lifetimeCtx context.Context, socketPath string, log logr.Logger) (UnixSocketNotificationSource, error) {
	listener, listenErr := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if listenErr != nil {
		return nil, fmt.Errorf("could not create notification socket at %s: %w", socketPath, listenErr)
	}

	ns := &unixSocketNotificationSource{
		lifetimeCtx:     lifetimeCtx,
		log:             log,
		socketPath:      socketPath,
		lock:            &sync.Mutex{},
		listener:        listener,
		subscriptions:   make(map[uint32]*concurrency.UnboundedChan[Notification]),
		dispose:         concurrency.NewOneTimeJob[struct{}](),
		clientConnected: concurrency.NewSemaphore(),
	}
	ns.subCtx, ns.subCtxCancel = context.WithCancel(context.Background())
	context.AfterFunc(lifetimeCtx, ns.disposeOnce)

	notifyServer := grpc.NewServer()
	proto.RegisterNotificationsServer(notifyServer, ns)

	go func() {
		serverErr := notifyServer.Serve(ns.listener)
		if serverErr != nil && !errors.Is(serverErr, net.ErrClosed) {
			ns.log.Error(serverErr, "Notification server encountered an error")
		}
	}()

	return ns, nil
}
