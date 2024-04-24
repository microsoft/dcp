package exerunners

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

	// Timeout for operations on the WebSocket connection to the IDE notification endpoint.
	operationTimeout = 20 * time.Second

	// The period for sending ping messages to detect stale IDE notification connections.
	// Must be smaller than operationTimeout.
	// This is also the period in which we will detect lifetime context cancellation and shut down the handler.
	pingPeriod = 5 * time.Second
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
	reportTimeoutErrors  bool
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

	// Before version20240423 the endpoint was not sending pong responses to ping messages, to timeouts are somewhat expected.
	retval.reportTimeoutErrors = equalOrNewer(connInfo.apiVersion, version20240423)

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
		if equalOrNewer(nh.connInfo.apiVersion, version20240303) {
			url = fmt.Sprintf("%s://localhost:%s%s?%s=%s", nh.connInfo.webSocketScheme, nh.connInfo.portStr, ideRunSessionNotificationResourcePath, queryParamApiVersion, nh.connInfo.apiVersion)
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
	connCtx, cancelConnCtx := context.WithCancel(nh.lifetimeCtx)
	defer cancelConnCtx()

	closeConn := func() {
		cancelConnCtx()
		nh.closeNotifySocket(sendClosingMessage)
	}

	closeConnAndReconnect := func(ioError error) {
		if nh.lifetimeCtx.Err() != nil {
			// We are being asked to shut down. Do not attempt to reconnect.
			_ = nh.setState(handlerStateAny, handlerStateDisposed, closeConn)
			return
		} else {
			stateTransitionErr := nh.setState(handlerStateConnected, handlerStateInitial, closeConn)
			if stateTransitionErr != nil {
				// Should never happen--we were in connected state, so the transition to initial statue should have succeeded
				nh.log.Error(errors.Join(ioError, stateTransitionErr), "failed to transition to initial state when processing error from reading IDE run session notification messages")
				_ = nh.setState(handlerStateAny, handlerStateDisposed, nil)
			} else {
				go nh.tryConnecting() // Attempt to reconnect
			}
		}
	}

	reportErrorAndReconnect := func(err error, msg string, keysAndValues ...any) {
		if connCtx.Err() == nil {
			// Only report unexpected errors (errors that are not due to context cancellation).
			if nh.reportTimeoutErrors || !isTimeout(err) {
				nh.log.V(1).Error(err, msg, keysAndValues...)
			}
		}

		closeConnAndReconnect(err)
	}

	setupErr := nh.notifySocket.SetReadDeadline(time.Now().Add(operationTimeout))
	if setupErr != nil {
		reportErrorAndReconnect(setupErr, "failed to set read deadline on IDE run session notification endpoint, recycling connection...")
		return
	}

	nh.notifySocket.SetPongHandler(func(string) error {
		// If we receive a pong response to our ping, it means the connection is alive, so we can extend the operation (read) deadline.
		deadline := time.Now().Add(operationTimeout)
		if connCtx.Err() != nil {
			// We are being asked to end the connection. Set the deadline to now to force a timeout and exit the message reading loop.
			deadline = time.Now()
		}
		return nh.notifySocket.SetReadDeadline(deadline)
	})

	go nh.doPinging(connCtx, nh.notifySocket)

	for {
		msgType, msg, msgReadErr := nh.notifySocket.ReadMessage()

		var closeErr *websocket.CloseError
		if errors.As(msgReadErr, &closeErr) {
			reportErrorAndReconnect(msgReadErr, "IDE run session notification endpoint closed the connection, recycling connection...")
			return
		}

		if msgReadErr != nil {
			reportErrorAndReconnect(msgReadErr, "failed to read message from IDE run session notification endpoint, recycling connection...")
			return
		}

		if connCtx.Err() != nil {
			closeConnAndReconnect(nil)
			return
		}

		// We received a message successfully and we are not asked to reconnect, so we can reset the read deadline.
		deadlineResetErr := nh.notifySocket.SetReadDeadline(time.Now().Add(operationTimeout))
		if deadlineResetErr != nil {
			reportErrorAndReconnect(deadlineResetErr, "failed to reset read deadline on IDE run session notification endpoint, recycling connection...")
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

func (nh *ideNotificationHandler) doPinging(connCtx context.Context, conn *websocket.Conn) {
	pingTimer := time.NewTimer(0)

	for {
		if connCtx.Err() != nil {
			pingTimer.Stop()
			return
		}

		pingTimer.Reset(pingPeriod)

		select {
		case <-connCtx.Done():
			pingTimer.Stop()
			return
		case <-pingTimer.C:
			pingErr := conn.WriteMessage(websocket.PingMessage, nil)
			if pingErr != nil {
				nh.log.V(1).Error(pingErr, "failed to send ping message to IDE run session notification endpoint")
			}
		}
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		return true
	}

	return false
}
