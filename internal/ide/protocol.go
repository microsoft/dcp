/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

// Package ide implements a client for the Aspire IDE execution protocol
// (https://github.com/microsoft/aspire/blob/main/docs/specs/IDE-execution.md).
// It is consumed by both the IdeExecutableRunner (for Executable objects with
// ExecutionType=IDE) and the IdeSession controller (for stand-alone IDE debug
// sessions).
package ide

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

// SessionID is the run-session identifier assigned by the IDE.
type SessionID string

// MessageLevel matches the severity levels defined by the IDE execution
// protocol for sessionMessage notifications.
type MessageLevel string

const (
	MessageLevelInfo  MessageLevel = "info"
	MessageLevelDebug MessageLevel = "debug"
	MessageLevelError MessageLevel = "error"
)

// StartSessionRequest is the data sent to the IDE to create a new run session.
type StartSessionRequest struct {
	// LaunchConfigurations is the raw JSON array of launch configurations to
	// hand to the IDE. At least one element must be present, and at least one
	// element's "type" must be in Client.SupportedLaunchConfigurations().
	LaunchConfigurations json.RawMessage

	// Optional environment variables passed to the launched process.
	Env []apiv1.EnvVar

	// Optional process arguments.
	Args []string
}

// EarlyTermination describes a session that terminated synchronously during
// StartSession (the IDE delivered a sessionTerminated notification before the
// HTTP "create session" response arrived).
type EarlyTermination struct {
	// ExitCode is the exit code reported by the IDE, or apiv1.UnknownExitCode if
	// the IDE did not provide one (or it was outside the int32 range).
	ExitCode *int32

	// Failed indicates whether the IDE reported the session as having failed to
	// start (the termination notification arrived without a processRestarted
	// notification first). When false, the session ran briefly and exited.
	Failed bool
}

// StartSessionResult is returned from a successful StartSession call.
type StartSessionResult struct {
	// SessionID is the IDE-assigned identifier for the session.
	SessionID SessionID

	// EarlyTermination is set when the session was reported as terminated by the
	// IDE before StartSession returned. When non-nil, no OnTerminated callback
	// will be delivered for this session (the caller already has the data) and
	// ConfirmHandlerReady is a no-op.
	EarlyTermination *EarlyTermination

	// ConfirmHandlerReady must be invoked by the caller once it has finished
	// any initial bookkeeping for the session and is ready to receive
	// notifications via the handler that was passed to StartSession.
	// Notifications received between StartSession returning and
	// ConfirmHandlerReady being called are buffered and delivered in order on
	// the session's dispatch goroutine. The function is safe to call multiple
	// times; only the first call has effect.
	ConfirmHandlerReady func()
}

// SessionHandler receives notifications about a single IDE run session.
// All methods for a given SessionID are invoked sequentially on the session's
// dispatch goroutine; implementations must therefore avoid blocking for long
// periods inside these callbacks.
type SessionHandler interface {
	// OnProcessChanged is invoked when the IDE reports a new (or restarted)
	// main process for the session. The IDE may report process changes
	// multiple times.
	OnProcessChanged(sessionID SessionID, pid process.Pid_t)

	// OnTerminated is invoked when the IDE reports the session as terminated.
	// exitCode is apiv1.UnknownExitCode if the IDE did not provide one (or it
	// did not fit into int32).
	OnTerminated(sessionID SessionID, exitCode *int32)

	// OnLog delivers a stdout/stderr line emitted by the session's process.
	OnLog(sessionID SessionID, isStdErr bool, message string)

	// OnMessage delivers a user-facing diagnostic message about the session.
	// For MessageLevelError the message contains a formatted representation of
	// the error code, message, and any nested error details supplied by the IDE.
	OnMessage(sessionID SessionID, level MessageLevel, message string)
}

// Wire-level protocol types follow. These are unexported because they are pure
// transport detail; consumers use Client and SessionHandler.

type notificationType string

const (
	notificationTypeProcessRestarted  notificationType = "processRestarted"
	notificationTypeSessionTerminated notificationType = "sessionTerminated"
	notificationTypeServiceLogs       notificationType = "serviceLogs"
	notificationTypeSessionMessage    notificationType = "sessionMessage"
)

type ideSessionNotificationBase struct {
	NotificationType notificationType `json:"notification_type"`
	SessionID        string           `json:"session_id,omitempty"`
}

type ideRunSessionProcessChangedNotification struct {
	ideSessionNotificationBase
	PID process.Pid_t `json:"pid,omitempty"`
}

func (pcn *ideRunSessionProcessChangedNotification) ToString() string {
	maybePID := ""
	if pcn.PID != 0 {
		maybePID = fmt.Sprintf(" (PID: %d)", pcn.PID)
	}
	return fmt.Sprintf("Session %s: %s%s", pcn.SessionID, pcn.NotificationType, maybePID)
}

type ideRunSessionTerminatedNotification struct {
	ideRunSessionProcessChangedNotification

	// The spec says exit_code is an unsigned 32-bit integer, but we use an int64 here just to be more resilient
	// in case someone sends us a negative value, or a value outside uint32 range.
	// This way we have a higher chance of deserializing the notification and correctly processing session termination.
	ExitCode *int64 `json:"exit_code,omitempty"`
}

type ideSessionLogNotification struct {
	ideSessionNotificationBase
	IsStdErr   bool   `json:"is_std_err"`
	LogMessage string `json:"log_message"`
}

type sessionMessageLevel string

const (
	sessionMessageLevelInfo  sessionMessageLevel = "info"
	sessionMessageLevelDebug sessionMessageLevel = "debug"
	sessionMessageLevelError sessionMessageLevel = "error"
)

type ideSessionMessageNotification struct {
	ideSessionNotificationBase
	Message string              `json:"message"`
	Level   sessionMessageLevel `json:"level"`
	Code    string              `json:"code,omitempty"`    // only for level=error
	Details []errorDetail       `json:"details,omitempty"` // only for level=error
}

type ideRunSessionRequestV1 struct {
	// This is typically an array of ideLaunchConfiguration-derived objects,
	// but in this implementation we will take whatever the caller supplies and
	// just pass it through to the IDE (which is why the type is json.RawMessage).
	// Must have at least one element.
	LaunchConfigurations json.RawMessage `json:"launch_configurations"`

	Env  []apiv1.EnvVar `json:"env,omitempty"`
	Args []string       `json:"args,omitempty"`
}

type launchConfigurationBase struct {
	Type string `json:"type"`
}

type errorDetail struct {
	Code    string        `json:"code"`
	Message string        `json:"message,omitempty"`
	Details []errorDetail `json:"details,omitempty"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

func (er *errorResponse) String() string {
	var b strings.Builder
	er.Error.writeDetails(&b, []byte(""))
	return b.String()
}

var errorDetailChildIndent = []byte(strings.Repeat(" ", 2))

func (ed *errorDetail) writeDetails(b *strings.Builder, indent []byte) {
	switch {
	case ed.Code != "" && ed.Message != "":
		fmt.Fprintf(b, "%s%s: %s", indent, ed.Code, ed.Message)
	case ed.Code != "":
		fmt.Fprintf(b, "%s%s", indent, ed.Code)
	case ed.Message != "":
		fmt.Fprintf(b, "%s%s", indent, ed.Message)
	}

	if len(ed.Details) > 0 {
		childIndent := append(indent, errorDetailChildIndent...)
		for _, detail := range ed.Details {
			b.Write(osutil.WithNewline(nil))
			detail.writeDetails(b, childIndent)
		}
	}
}

type infoResponse struct {
	ProtocolsSupported                []apiVersion `json:"protocols_supported"`
	SupportedLaunchConfigurationTypes []string     `json:"supported_launch_configurations,omitempty"`
}

type apiVersion string

const (
	ideRunSessionResourcePath             = "/run_session"
	ideRunSessionNotificationResourcePath = "/run_session/notify"
	ideRunSessionInfoPath                 = "/info"

	ideEndpointPortVar  = "DEBUG_SESSION_PORT"
	ideEndpointTokenVar = "DEBUG_SESSION_TOKEN"
	ideEndpointCertVar  = "DEBUG_SESSION_SERVER_CERTIFICATE"

	version20240303      apiVersion = "2024-03-03"
	version20240423      apiVersion = "2024-04-23"
	version20251001      apiVersion = "2025-10-01"
	queryParamApiVersion            = "api-version"
	instanceIdHeader                = "Microsoft-Developer-DCP-Instance-ID"

	// Well-known launch configurations
	vsProjectLaunchConfiguration = "project"

	// Environment variables used to override IDE-related timeouts at runtime.
	DCP_IDE_REQUEST_TIMEOUT_SECONDS        = "DCP_IDE_REQUEST_TIMEOUT_SECONDS"
	DCP_IDE_NOTIFICATION_TIMEOUT_SECONDS   = "DCP_IDE_NOTIFICATION_TIMEOUT_SECONDS"
	DCP_IDE_NOTIFICATION_KEEPALIVE_SECONDS = "DCP_IDE_NOTIFICATION_KEEPALIVE_SECONDS"
)

var (
	ideEndpointRequestTimeout = 120 * time.Second
)

func equalOrNewer(currentVersion, baselineVersion apiVersion) bool {
	currentVersionTime, parseErr := time.Parse(time.DateOnly, string(currentVersion))
	if parseErr != nil {
		return false
	}
	baselineVersionTime, parseErr := time.Parse(time.DateOnly, string(baselineVersion))
	if parseErr != nil {
		return false
	}
	return currentVersionTime.After(baselineVersionTime) || currentVersionTime.Equal(baselineVersionTime)
}

func init() {
	ideRequestTimeoutOverride, found := osutil.EnvVarIntVal(DCP_IDE_REQUEST_TIMEOUT_SECONDS)
	if found && ideRequestTimeoutOverride > 0 {
		ideEndpointRequestTimeout = time.Duration(ideRequestTimeoutOverride) * time.Second
	}
}
