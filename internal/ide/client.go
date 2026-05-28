/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package ide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const (
	runSessionCouldNotBeStarted = "run session could not be started: "
	runSessionCouldNotBeStopped = "run session could not be stopped: "

	// stopTerminationTimeout is the maximum amount of time StopSession will
	// wait for a session-terminated notification from the IDE after a
	// successful DELETE response before giving up and treating the session as
	// terminated.
	stopTerminationTimeout = 10 * time.Second
)

// Client talks to the Aspire IDE execution endpoint, drives the lifecycle of
// IDE-managed run sessions, and dispatches per-session notifications to the
// SessionHandler the caller supplies at StartSession time. A single Client
// instance is intended to be shared by every component that needs to manage
// IDE-backed sessions in a single DCP controller-manager process.
type Client struct {
	lifetimeCtx         context.Context
	connInfo            *connectionInfo
	notificationHandler *notificationHandler
	activeSessions      *syncmap.Map[SessionID, *sessionData]
	log                 logr.Logger
}

// NewClient constructs a Client. It performs the IDE /info handshake to
// negotiate the protocol version, so it returns an error if the IDE endpoint
// is not configured (missing DEBUG_SESSION_* environment variables) or no
// compatible protocol version can be negotiated. The lifetimeCtx controls
// background activity (notification handler reconnects, dispatch queues) and
// must remain valid for as long as the Client is used.
func NewClient(lifetimeCtx context.Context, log logr.Logger) (*Client, error) {
	connInfo, err := newConnectionInfo(lifetimeCtx, log)
	if err != nil {
		return nil, err
	}

	c := &Client{
		lifetimeCtx:    lifetimeCtx,
		connInfo:       connInfo,
		activeSessions: &syncmap.Map[SessionID, *sessionData]{},
		log:            log,
	}
	c.notificationHandler = newNotificationHandler(lifetimeCtx, c, connInfo, log)
	return c, nil
}

// SupportedLaunchConfigurations returns the launch-configuration "type"
// values advertised by the connected IDE.
func (c *Client) SupportedLaunchConfigurations() []string {
	return c.connInfo.supportedLaunchConfigurations
}

// ValidateLaunchConfigurations parses the supplied launch-configurations
// payload, ensures it is a JSON array with at least one object that has a
// "type" supported by the connected IDE, and returns nil on success.
func (c *Client) ValidateLaunchConfigurations(launchConfigurations json.RawMessage) error {
	if len(launchConfigurations) == 0 {
		return errors.New("launch configurations are required")
	}

	var lcs []launchConfigurationBase
	unmarshalErr := json.Unmarshal(launchConfigurations, &lcs)
	if unmarshalErr != nil {
		return fmt.Errorf("launch configurations are invalid: %w", unmarshalErr)
	}
	if len(lcs) == 0 {
		return errors.New("at least one launch configuration is required")
	}

	requestedConfigurations := slices.Map[string](lcs, func(lc launchConfigurationBase) string { return lc.Type })
	if len(slices.Intersect(c.connInfo.supportedLaunchConfigurations, requestedConfigurations)) == 0 {
		return fmt.Errorf("none of the requested launch configuration types is supported by the connected IDE; supported types: %v, requested types: %v",
			c.connInfo.supportedLaunchConfigurations, requestedConfigurations)
	}
	return nil
}

// StartSession sends a "create run session" request to the IDE and registers
// the supplied handler to receive notifications about the resulting session.
// It blocks until the HTTP request completes (or fails).
//
// Notifications about the session may arrive between StartSession returning
// and the consumer being ready to handle them; for that reason every
// successful (non-EarlyTermination) result includes a ConfirmHandlerReady
// callback that the caller MUST invoke once it is ready, even if it has
// nothing to do beyond observing the returned SessionID. Notifications are
// buffered on the session's serial dispatch queue and delivered to the
// handler in order once ConfirmHandlerReady has been called.
//
// If the IDE delivers a session-terminated notification before the HTTP
// response arrives, the returned result's EarlyTermination field is set,
// the handler is never invoked, and the caller does not need to call
// ConfirmHandlerReady (it is a no-op) or ReleaseSession (the session is
// already cleaned up internally).
func (c *Client) StartSession(
	ctx context.Context,
	req StartSessionRequest,
	handler SessionHandler,
	log logr.Logger,
) (*StartSessionResult, error) {
	if handler == nil {
		return nil, errors.New(runSessionCouldNotBeStarted + "session handler is required")
	}

	if err := c.notificationHandler.WaitConnected(ctx); err != nil {
		return nil, fmt.Errorf(runSessionCouldNotBeStarted+"%w", err)
	}

	if err := c.ValidateLaunchConfigurations(req.LaunchConfigurations); err != nil {
		return nil, fmt.Errorf(runSessionCouldNotBeStarted+"%w", err)
	}

	if !equalOrNewer(c.connInfo.apiVersion, version20240303) {
		return nil, fmt.Errorf(runSessionCouldNotBeStarted+
			"Aspire IDE extension is older than the minimum supported version; DCP requires an extension that supports protocol version %s or newer",
			version20240303)
	}

	body, marshalErr := json.Marshal(ideRunSessionRequestV1{
		LaunchConfigurations: req.LaunchConfigurations,
		Env:                  req.Env,
		Args:                 req.Args,
	})
	if marshalErr != nil {
		return nil, fmt.Errorf(runSessionCouldNotBeStarted+"failed to create request body: %w", marshalErr)
	}

	httpReq, reqCancel, reqErr := c.connInfo.makeRequest(c.lifetimeCtx, ideRunSessionResourcePath, http.MethodPut, bytes.NewBuffer(body))
	if reqErr != nil {
		return nil, fmt.Errorf(runSessionCouldNotBeStarted+"failed to create request: %w", reqErr)
	}
	defer reqCancel()

	if rawRequest, dumpErr := httputil.DumpRequest(httpReq, true); dumpErr == nil {
		log.V(1).Info("Sending IDE run session request", "Request", string(rawRequest))
	} else {
		log.V(1).Info("Sending IDE run session request", "URL", httpReq.URL)
	}

	resp, doErr := c.connInfo.httpClient.Do(httpReq)
	if doErr != nil {
		if errors.Is(doErr, context.DeadlineExceeded) {
			log.Error(doErr, fmt.Sprintf("Timeout of %.0f seconds exceeded waiting for the IDE to start a run session; you can set the %s environment variable to override this timeout (in seconds)", ideEndpointRequestTimeout.Seconds(), DCP_IDE_REQUEST_TIMEOUT_SECONDS))
		}
		return nil, fmt.Errorf(runSessionCouldNotBeStarted+"%w", doErr)
	}
	defer resp.Body.Close()

	if rawResponse, dumpErr := httputil.DumpResponse(resp, true); dumpErr == nil {
		log.V(1).Info("Completed IDE run session request", "Response", string(rawResponse))
	} else {
		log.V(1).Info("Completed IDE run session request", "URL", httpReq.URL)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body) // Best effort to get as much detail about the error as possible
		return nil, fmt.Errorf(runSessionCouldNotBeStarted+"IDE returned unexpected status: %s %s", resp.Status, parseResponseBody(respBody))
	}

	sessionURL := resp.Header.Get("Location")
	if sessionURL == "" {
		return nil, errors.New(runSessionCouldNotBeStarted + "IDE response is missing required 'Location' header")
	}
	sidStr, urlErr := getLastUrlPathSegment(sessionURL)
	if urlErr != nil {
		return nil, fmt.Errorf(runSessionCouldNotBeStarted+"%w", urlErr)
	}
	sessionID := SessionID(sidStr)

	// Notifications about the session may have arrived (and created a sessionData entry)
	// before the HTTP response did, so use LoadOrStoreNew.
	sd := c.ensureSession(sessionID)

	sd.lock.Lock()
	if sd.state == sessionStateFailedToStart || sd.state == sessionStateCompleted {
		// The IDE has already sent a termination notification for this session.
		// Synthesize an EarlyTermination result; the handler is never registered
		// or invoked, and the session is removed from active sessions.
		et := &EarlyTermination{
			ExitCode: pointers.Duplicate(sd.exitCode),
			Failed:   sd.state == sessionStateFailedToStart,
		}
		// Release both readiness counts so the queued dispatch workers can exit cleanly.
		sd.releaseHandlerReady(requiredHandlerReadiness)
		sd.lock.Unlock()

		c.activeSessions.Delete(sessionID)

		log.V(1).Info("IDE run session terminated during creation", "SessionID", sessionID, "Failed", et.Failed)
		return &StartSessionResult{
			SessionID:           sessionID,
			EarlyTermination:    et,
			ConfirmHandlerReady: func() {},
		}, nil
	}

	sd.handler = handler
	sd.lock.Unlock()

	// First half of the readiness gate: handler is now set.
	sd.releaseHandlerReady(1)

	log.V(1).Info("IDE run session started", "SessionID", sessionID)

	var confirmOnce sync.Once
	confirm := func() {
		confirmOnce.Do(func() {
			sd.releaseHandlerReady(1)
		})
	}

	return &StartSessionResult{
		SessionID:           sessionID,
		ConfirmHandlerReady: confirm,
	}, nil
}

// StopSession asks the IDE to terminate the named session and blocks until
// the IDE delivers a session-terminated notification or stopTerminationTimeout
// elapses. The caller's SessionHandler may receive OnTerminated either before
// or after StopSession returns.
//
// StopSession does not remove the session from the Client's internal map;
// callers should call ReleaseSession after they are done with the session
// (typically once they have observed OnTerminated).
func (c *Client) StopSession(ctx context.Context, sessionID SessionID, log logr.Logger) error {
	sd, found := c.activeSessions.Load(sessionID)
	if !found {
		log.V(1).Info("Attempted to stop IDE run session which was not found", "SessionID", sessionID)
		return nil
	}

	httpReq, reqCancel, reqErr := c.connInfo.makeRequest(c.lifetimeCtx, ideRunSessionResourcePath+"/"+string(sessionID), http.MethodDelete, nil)
	if reqErr != nil {
		return fmt.Errorf(runSessionCouldNotBeStopped+"failed to create request: %w", reqErr)
	}
	defer reqCancel()

	resp, doErr := c.connInfo.httpClient.Do(httpReq)
	if doErr != nil {
		return fmt.Errorf(runSessionCouldNotBeStopped+"%w", doErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.V(1).Info("IDE run session stop requested; waiting for IDE to confirm termination", "SessionID", sessionID)

		waitCtx, waitCancel := context.WithTimeout(ctx, stopTerminationTimeout)
		defer waitCancel()

		select {
		case <-waitCtx.Done():
			// The IDE did not deliver a termination notification within the
			// timeout. Synthesize one so callers waiting on the handler wake up
			// and so the per-session state is consistent.
			log.V(1).Info("Timeout waiting for IDE to confirm run session termination; synthesizing termination", "SessionID", sessionID)
			c.synthesizeTermination(sd, sessionID)
		case <-sd.exitCh:
			// Termination notification already received from the IDE.
		}
		return nil
	}

	if resp.StatusCode == http.StatusNoContent {
		log.Info("Attempted to stop IDE run session which was not found", "SessionID", sessionID)
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body) // Best effort to get as much detail about the error as possible
	return fmt.Errorf(runSessionCouldNotBeStopped+"%s %s", resp.Status, parseResponseBody(respBody))
}

// ReleaseSession removes any internal bookkeeping the Client holds for the
// session. It is safe to call multiple times and on sessions the Client has
// no record of. Callers MUST call ReleaseSession when they are done with a
// session (typically after observing OnTerminated, or after StopSession
// returns).
func (c *Client) ReleaseSession(_ context.Context, sessionID SessionID, log logr.Logger) error {
	sd, found := c.activeSessions.LoadAndDelete(sessionID)
	if !found {
		log.V(1).Info("Release of an IDE run session requested, but the session was already released", "SessionID", sessionID)
		return nil
	}
	// If the handler was never registered (we are releasing a session that the
	// caller never managed to fully start), release the readiness gate so any
	// pending dispatch workers can exit.
	sd.lock.Lock()
	handlerWasSet := sd.handler != nil
	sd.lock.Unlock()
	if !handlerWasSet {
		sd.releaseHandlerReady(requiredHandlerReadiness)
	}
	return nil
}

// synthesizeTermination records a session-terminated state for sessions where
// the IDE failed to deliver a real termination notification (e.g. StopSession
// timeout). The handler receives OnTerminated with an unknown exit code so
// callers can complete their cleanup.
func (c *Client) synthesizeTermination(sd *sessionData, sessionID SessionID) {
	sd.lock.Lock()
	if sd.state == sessionStateCompleted || sd.state == sessionStateFailedToStart {
		sd.lock.Unlock()
		return
	}
	sd.state = sessionStateCompleted
	sd.exitCode = apiv1.UnknownExitCode
	close(sd.exitCh)
	sd.lock.Unlock()

	sd.dispatch(func(handler SessionHandler) {
		handler.OnTerminated(sessionID, apiv1.UnknownExitCode)
	})
}

// ensureSession returns the sessionData for the given session ID, creating it
// if necessary. Notifications about a session can arrive before the HTTP
// "create session" response, so this is used by both the StartSession success
// path and the notification handlers.
func (c *Client) ensureSession(sessionID SessionID) *sessionData {
	sd, _ := c.activeSessions.LoadOrStoreNew(sessionID, func() *sessionData {
		return newSessionData(c.lifetimeCtx)
	})
	return sd
}

// notificationReceiver implementation -------------------------------------------------------

func (c *Client) handleSessionChange(pcn ideRunSessionProcessChangedNotification) {
	sessionID := SessionID(pcn.SessionID)
	c.log.V(1).Info("IDE run session changed", "SessionID", sessionID, "PID", pcn.PID)

	sd := c.ensureSession(sessionID)
	sd.lock.Lock()
	sd.pid = pcn.PID
	if sd.state == sessionStateNotStarted {
		sd.state = sessionStateRunning
	}
	pid := pcn.PID
	sd.lock.Unlock()

	sd.dispatch(func(handler SessionHandler) {
		handler.OnProcessChanged(sessionID, pid)
	})
}

func (c *Client) handleSessionTermination(stn ideRunSessionTerminatedNotification) {
	sessionID := SessionID(stn.SessionID)
	exitCode := exitCodeFromInt64(stn.ExitCode, c.log, sessionID)
	c.log.V(1).Info("IDE run session terminated", "SessionID", sessionID, "PID", stn.PID, "ExitCode", exitCode)

	sd := c.ensureSession(sessionID)
	sd.lock.Lock()
	sd.pid = stn.PID
	sd.exitCode = exitCode
	switch sd.state {
	case sessionStateNotStarted:
		sd.state = sessionStateFailedToStart
		close(sd.exitCh)
	case sessionStateRunning:
		sd.state = sessionStateCompleted
		close(sd.exitCh)
	}
	sd.lock.Unlock()

	sd.dispatch(func(handler SessionHandler) {
		handler.OnTerminated(sessionID, pointers.Duplicate(exitCode))
	})
}

func (c *Client) handleServiceLogs(nsl ideSessionLogNotification) {
	sessionID := SessionID(nsl.SessionID)
	sd := c.ensureSession(sessionID)
	isStdErr := nsl.IsStdErr
	message := nsl.LogMessage
	sd.dispatch(func(handler SessionHandler) {
		handler.OnLog(sessionID, isStdErr, message)
	})
}

func (c *Client) handleSessionMessage(smn ideSessionMessageNotification) {
	sessionID := SessionID(smn.SessionID)
	log := c.log.WithValues("SessionID", sessionID)

	msg := strings.TrimSpace(smn.Message)
	if msg == "" {
		log.V(1).Info("Received empty IDE session message, ignoring")
		return
	}

	sd := c.ensureSession(sessionID)

	switch smn.Level {
	case sessionMessageLevelInfo:
		sd.dispatch(func(handler SessionHandler) {
			handler.OnMessage(sessionID, MessageLevelInfo, msg)
		})
	case sessionMessageLevelDebug:
		sd.dispatch(func(handler SessionHandler) {
			handler.OnMessage(sessionID, MessageLevelDebug, msg)
		})
	case sessionMessageLevelError:
		er := errorResponse{
			Error: errorDetail{
				Code:    smn.Code,
				Message: smn.Message,
				Details: smn.Details,
			},
		}
		errMsg := er.String()
		sd.dispatch(func(handler SessionHandler) {
			handler.OnMessage(sessionID, MessageLevelError, errMsg)
		})
	default:
		log.V(1).Info("Received IDE session message with unexpected severity level", "Level", smn.Level, "Message", smn.Message)
	}
}

// Helpers -----------------------------------------------------------------------------------

func exitCodeFromInt64(raw *int64, log logr.Logger, sessionID SessionID) *int32 {
	if raw == nil {
		return apiv1.UnknownExitCode
	}
	if math.MinInt32 <= *raw && *raw <= math.MaxUint32 {
		// If the exit code can be represented as int32 without data loss, use it.
		// A reinterpretation of uint32 value will occur if the code > math.MaxInt32, but that is acceptable.
		ec := int32(*raw)
		return &ec
	}
	log.Info("Received IDE run session termination notification with exit code outside uint32 range; will treat exit code as 'unknown'.",
		"SessionID", sessionID,
		"ReceivedExitCode", *raw,
	)
	return apiv1.UnknownExitCode
}

func getLastUrlPathSegment(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	pathSegments := strings.Split(u.Path, "/")
	if len(pathSegments) == 0 {
		return "", fmt.Errorf("URL '%s' has no path segments", rawURL)
	}
	return pathSegments[len(pathSegments)-1], nil
}

func parseResponseBody(rawBody []byte) string {
	if len(rawBody) == 0 {
		return ""
	}

	var errResp errorResponse
	err := json.Unmarshal(rawBody, &errResp)
	if err == nil {
		return errResp.String()
	} else {
		return string(rawBody)
	}
}

// Ensure Client satisfies the internal notificationReceiver interface.
var _ notificationReceiver = (*Client)(nil)
