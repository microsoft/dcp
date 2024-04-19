package exerunners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"

	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/concurrency"
)

type ideNotificationHandlerState uint32

const (
	handlerStateInitial    ideNotificationHandlerState = 0x1
	handlerStateConnecting ideNotificationHandlerState = 0x2
	handlerStateConnected  ideNotificationHandlerState = 0x4
	handlerStateDisposed   ideNotificationHandlerState = 0x8
	handlerStateAny        ideNotificationHandlerState = 0xFFFFFFFF

	// Timeout for reading messages from the IDE run session notification endpoint.
	// This is just a read operation timeout, allowing us to check whether we are being asked to shut down.
	defaultReadTimeout = 3 * time.Second
)

func (s ideNotificationHandlerState) String() string {
	switch s {
	case handlerStateInitial:
		return "Initial"
	case handlerStateConnecting:
		return "Connecting"
	case handlerStateConnected:
		return "Connected"
	case handlerStateDisposed:
		return "Disposed"
	default:
		return "Unknown"
	}
}

type ideNotificationRecevier interface {
	HandleSessionChange(pcn ideRunSessionProcessChangedNotification)
	HandleSessionTermination(pcn ideRunSessionTerminatedNotification)
	HandleServiceLogs(log ideSessionLogNotification)
}

// The IDE notification handler takes care of handling IDE run session notifications arriving via a WebSocket connection.
// It handles details of managing the connection and receiving notifications, allowing the IDE executable runner
// to focus on starting Executables and handling their lifetime events.
type ideNotificationHandler struct {
	lock                 *sync.Mutex
	lifetimeCtx          context.Context
	notificationReceiver ideNotificationRecevier // The receiver of IDE run session notifications (the IDE runner)
	state                ideNotificationHandlerState
	stateChanged         *concurrency.AutoResetEvent // Set when state changes
	log                  logr.Logger
	notifySocket         *websocket.Conn
	connInfo             *ideConnectionInfo
}

func NewIdeNotificationHandler(
	lifetimeCtx context.Context,
	notificationReceiver ideNotificationRecevier,
	connInfo *ideConnectionInfo,
	log logr.Logger,
) *ideNotificationHandler {
	retval := &ideNotificationHandler{
		lock:                 &sync.Mutex{},
		lifetimeCtx:          lifetimeCtx,
		notificationReceiver: notificationReceiver,
		state:                handlerStateInitial,
		stateChanged:         concurrency.NewAutoResetEvent(false),
		log:                  log,
		connInfo:             connInfo,
	}

	return retval
}

func (nh *ideNotificationHandler) WaitConnected(ctx context.Context) error {
	nhState := nh.getState()
	if nhState == handlerStateConnected {
		return nil
	} else {
		go nh.tryConnecting()
	}

	for {
		nhState = nh.getState()
		if nhState == handlerStateDisposed {
			return fmt.Errorf("the IDE session endpoint is not available")
		}
		if nhState == handlerStateConnected {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for IDE session endpoint to become available")
		case <-nh.stateChanged.Wait():
			// Continue the loop
		}
	}
}

// Returns the state of the IDE notification handler.
func (nh *ideNotificationHandler) getState() ideNotificationHandlerState {
	nh.lock.Lock()
	defer nh.lock.Unlock()
	return nh.state
}

// Transition the IDE notification handler to a new state, if the current state matches the expected state.
// If the transition is successful, the provided action (if any) is executed.
func (nh *ideNotificationHandler) setState(expectedState, newState ideNotificationHandlerState, action func()) error {
	nh.lock.Lock()
	defer nh.lock.Unlock()
	if nh.state == newState {
		if action != nil {
			action()
		}
		return nil
	}
	if nh.state&expectedState != 0 {
		nh.state = newState
		if action != nil {
			action()
		}
		nh.stateChanged.Set()

		if newState == handlerStateDisposed {
			nh.log.V(1).Info("IDE connection handler has been disposed. No further notifications will be received.")
		}

		return nil
	}
	return fmt.Errorf("IDE connection handler cannot transition from state %s to state %s", nh.state.String(), newState.String())
}

// Retry connecting to the IDE notification socket until we succeed or the lifetime context is cancelled.
func (nh *ideNotificationHandler) tryConnecting() {
	stateTransitionErr := nh.setState(handlerStateInitial, handlerStateConnecting, nil)
	if stateTransitionErr != nil {
		// This is expected: we might be already connecting, or already connected, or disposed.
		// In any of these cases, we should not do anything beyond what we are already doing.
		return
	}

	retryPolicy := backoff.NewExponentialBackOff()
	retryPolicy.MaxInterval = 20 * time.Second
	retryPolicy.MaxElapsedTime = 0 // Only stop retrying when the lifetime context is cancelled

	socket, retryErr := resiliency.RetryGet(nh.lifetimeCtx, retryPolicy, func() (*websocket.Conn, error) {
		headers := http.Header{}
		headers.Add("Authorization", fmt.Sprintf("Bearer %s", nh.connInfo.tokenStr))
		headers.Add(instanceIdHeader, nh.connInfo.instanceId)
		var url string
		if nh.connInfo.apiVersion == version20240303 {
			url = fmt.Sprintf("%s://localhost:%s%s?%s=%s", nh.connInfo.webSocketScheme, nh.connInfo.portStr, ideRunSessionNotificationResourcePath, queryParamApiVersion, version20240303)
		} else {
			url = fmt.Sprintf("%s://localhost:%s%s", nh.connInfo.webSocketScheme, nh.connInfo.portStr, ideRunSessionNotificationResourcePath)
		}

		conn, _, err := nh.connInfo.GetDialer().Dial(url, headers)
		if err == nil {
			return conn, nil
		} else {
			nh.log.V(1).Error(err, "failed to connect to IDE run session notification endpoint, retrying...")
			return nil, err
		}
	})

	if retryErr != nil {
		// We are shutting down, or the retry policy has given up.
		if !errors.Is(retryErr, context.Canceled) && !errors.Is(retryErr, context.DeadlineExceeded) {
			nh.log.Error(retryErr, "failed to connect to IDE run session notification endpoint")
		}
		_ = nh.setState(handlerStateAny, handlerStateDisposed, nil)
		return
	}

	stateTransitionErr = nh.setState(handlerStateConnecting, handlerStateConnected, func() { nh.notifySocket = socket })
	if stateTransitionErr != nil {
		// Should never happen
		nh.log.Error(stateTransitionErr, "failed to transition to connected state after connecting to IDE run session notification endpoint")
		_ = nh.setState(handlerStateAny, handlerStateDisposed, func() { nh.closeNotifySocket(noClosingMessage) })
		return
	}

	go nh.receiveNotifications()
}

const (
	sendClosingMessage = true
	noClosingMessage   = false
)

// Sends the Close WebSocket message to the IDE endpoint and closes the connection.
// Assumes the nh.lock is held during execution
func (nh *ideNotificationHandler) closeNotifySocket(sendClosingMessage bool) {
	if nh.notifySocket != nil {
		if sendClosingMessage {
			closeMsgErr := nh.notifySocket.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(100*time.Millisecond),
			)
			if closeMsgErr != nil {
				nh.log.V(1).Error(closeMsgErr, "failed to send close message to IDE run session notification endpoint")
			}
		}

		closeErr := nh.notifySocket.Close()
		if closeErr != nil {
			nh.log.V(1).Error(closeErr, "failed to close IDE run session notification endpoint")
		}
		nh.notifySocket = nil
	}
}

func (nh *ideNotificationHandler) receiveNotifications() {
	reportErrorAndReconnect := func(err error, msg string, keysAndValues ...any) {
		nh.log.V(1).Error(err, msg, keysAndValues...)
		stateTransitionErr := nh.setState(handlerStateConnected, handlerStateInitial, func() { nh.closeNotifySocket(sendClosingMessage) })
		if stateTransitionErr != nil {
			// Should never happen--we were in connected state, so the transition to initial statue should have succeeded
			nh.log.Error(errors.Join(err, stateTransitionErr), "failed to transition to initial state when processing error from reading IDE run session notification messages")
			_ = nh.setState(handlerStateAny, handlerStateDisposed, nil)
		} else {
			go nh.tryConnecting() // Attempt to reconnect
		}
	}

	for {
		if nh.lifetimeCtx.Err() != nil {
			nh.log.V(1).Info("IDE run session notification handler is being shut down...")
			_ = nh.setState(handlerStateAny, handlerStateDisposed, func() { nh.closeNotifySocket(sendClosingMessage) })
			return
		}

		readDeadlineErr := nh.notifySocket.SetReadDeadline(time.Now().Add(defaultReadTimeout))
		if readDeadlineErr != nil {
			reportErrorAndReconnect(readDeadlineErr, "failed to set read deadline on IDE run session notification endpoint, recycling connection...")
			return
		}

		msgType, msg, msgReadErr := nh.notifySocket.ReadMessage()
		if errors.Is(msgReadErr, os.ErrDeadlineExceeded) {
			continue // Opportunity to check if we are being asked to shut down
		}

		var closeErr *websocket.CloseError
		if errors.As(msgReadErr, &closeErr) {
			reportErrorAndReconnect(msgReadErr, "IDE run session notification endpoint closed the connection, recycling connection...")
			return
		}

		if msgReadErr != nil {
			reportErrorAndReconnect(msgReadErr, "failed to read message from IDE run session notification endpoint, recycling connection...")
			return
		}

		switch msgType {
		// No need to handle Ping and Pong messages, as the Gorilla WebSocket library handles them for us
		// The close message is reported as CloseError from ReadMessage() call, handled above

		case websocket.TextMessage:
			var basicNotification ideSessionNotificationBase
			unmarshalErr := json.Unmarshal(msg, &basicNotification)
			if unmarshalErr != nil {
				reportErrorAndReconnect(unmarshalErr, "invalid IDE basic session notification received, recycling connection...")
				return
			}

			if basicNotification.SessionID == "" {
				reportErrorAndReconnect(fmt.Errorf("received IDE run session notification with empty session ID"), "recycling connection...")
				return
			}

			switch basicNotification.NotificationType {
			case notificationTypeProcessRestarted:
				var pcn ideRunSessionProcessChangedNotification
				unmarshalErr = json.Unmarshal(msg, &pcn)
				if unmarshalErr != nil {
					reportErrorAndReconnect(unmarshalErr, "invalid IDE run session notification received, recycling connection...")
					return
				} else {
					nh.notificationReceiver.HandleSessionChange(pcn)
				}

			case notificationTypeSessionTerminated:
				var stn ideRunSessionTerminatedNotification
				unmarshalErr = json.Unmarshal(msg, &stn)
				if unmarshalErr != nil {
					reportErrorAndReconnect(unmarshalErr, "invalid IDE run session notification received, recycling connection...")
					return
				} else {
					nh.notificationReceiver.HandleSessionTermination(stn)
				}

			case notificationTypeServiceLogs:
				var nsl ideSessionLogNotification
				unmarshalErr = json.Unmarshal(msg, &nsl)
				if unmarshalErr != nil {
					reportErrorAndReconnect(unmarshalErr, "invalid IDE run session notification received, recycling connection...")
					return
				} else {
					nh.notificationReceiver.HandleServiceLogs(nsl)
				}
			}

		default:
			nh.log.Info("unexpected message type '%c' received from session notification endpoint, ignoring...", msgType)
		}
	}
}
