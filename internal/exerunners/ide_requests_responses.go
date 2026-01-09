/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/process"
)

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
	retval := fmt.Sprintf("Session %s: %s%s", pcn.SessionID, pcn.NotificationType, maybePID)
	return retval
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
	// but in this implementation we will take whatever the model annotation contains
	// and just pass it through to the IDE (that is why the type is json.RawMessage).
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

	launchConfigurationsAnnotation = "executable.usvc-dev.developer.microsoft.com/launch-configurations"

	version20240303      apiVersion = "2024-03-03"
	version20240423      apiVersion = "2024-04-23"
	version20251001      apiVersion = "2025-10-01"
	queryParamApiVersion            = "api-version"
	instanceIdHeader                = "Microsoft-Developer-DCP-Instance-ID"

	// Well-known launch configurations
	vsProjectLaunchConfiguration = "project"

	DCP_IDE_REQUEST_TIMEOUT_SECONDS        = "DCP_IDE_REQUEST_TIMEOUT_SECONDS"
	DCP_IDE_NOTIFICATION_TIMEOUT_SECONDS   = "DCP_IDE_NOTIFICATION_TIMEOUT_SECONDS"
	DCP_IDE_NOTIFICATION_KEEPALIVE_SECONDS = "DCP_IDE_NOTIFICATION_KEEPALIVE_SECONDS"
)

var (
	ideEndpointRequestTimeout = 120 * time.Second
)

func equalOrNewer(currentVersion, baselineVersion apiVersion) bool {
	currentVersionTime, paraseErr := time.Parse(time.DateOnly, string(currentVersion))
	if paraseErr != nil {
		return false
	}
	baselineVersionTime, paraseErr := time.Parse(time.DateOnly, string(baselineVersion))
	if paraseErr != nil {
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
