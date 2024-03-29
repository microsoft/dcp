package exerunners

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
)

type notificationType string

const (
	notificationTypeProcessRestarted  notificationType = "processRestarted"
	notificationTypeSessionTerminated notificationType = "sessionTerminated"
	notificationTypeServiceLogs       notificationType = "serviceLogs"
)

type ideSessionNotificationBase struct {
	NotificationType notificationType `json:"notification_type"`
	SessionID        string           `json:"session_id,omitempty"`
}

type ideRunSessionProcessChangedNotification struct {
	ideSessionNotificationBase
	PID process.Pid_t `json:"pid,omitempty"`
}

type ideRunSessionTerminatedNotification struct {
	ideRunSessionProcessChangedNotification
	ExitCode *int32 `json:"exit_code,omitempty"`
}

type ideSessionLogNotification struct {
	ideSessionNotificationBase
	IsStdErr   bool   `json:"is_std_err"`
	LogMessage string `json:"log_message"`
}

func (pcn *ideRunSessionProcessChangedNotification) ToString() string {
	maybePID := ""
	if pcn.PID != 0 {
		maybePID = fmt.Sprintf(" (PID: %d)", pcn.PID)
	}
	retval := fmt.Sprintf("Session %s: %s%s", pcn.SessionID, pcn.NotificationType, maybePID)
	return retval
}

type launchConfigurationType string

const (
	launchConfigurationTypeProject launchConfigurationType = "project"
)

type ideLaunchConfiguration struct {
	Type launchConfigurationType `json:"type"`
}

type projectLaunchMode string

const (
	projectLaunchModeDebug   projectLaunchMode = "Debug"
	projectLaunchModeNoDebug projectLaunchMode = "NoDebug"
)

type projectLaunchConfiguration struct {
	ideLaunchConfiguration
	ProjectPath          string            `json:"project_path"`
	LaunchMode           projectLaunchMode `json:"mode,omitempty"`
	LaunchProfile        string            `json:"launch_profile,omitempty"`
	DisableLaunchProfile bool              `json:"disable_launch_profile,omitempty"`
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

// This struct is defined solely for the purpose of supporting the transition between
// the old and new IDE protocol. Will be removed in future release.
type ideRunSessionRequestV1Explicit struct {
	LaunchConfigurations []projectLaunchConfiguration `json:"launch_configurations"`

	Env  []apiv1.EnvVar `json:"env,omitempty"`
	Args []string       `json:"args,omitempty"`
}

// Deprecated, will be removed in future release
type ideRunSessionRequestDeprecated struct {
	ProjectPath          string         `json:"project_path"`
	Debug                bool           `json:"debug,omitempty"`
	Env                  []apiv1.EnvVar `json:"env,omitempty"`
	Args                 []string       `json:"args,omitempty"`
	LaunchProfile        string         `json:"launch_profile,omitempty"`
	DisableLaunchProfile bool           `json:"disable_launch_profile,omitempty"`
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
	ProtocolsSupported []string `json:"protocols_supported"`
}

const (
	ideRunSessionResourcePath             = "/run_session"
	ideRunSessionNotificationResourcePath = "/run_session/notify"
	ideRunSessionInfoPath                 = "/info"

	ideEndpointPortVar  = "DEBUG_SESSION_PORT"
	ideEndpointTokenVar = "DEBUG_SESSION_TOKEN"
	ideEndpointCertVar  = "DEBUG_SESSION_SERVER_CERTIFICATE"

	launchConfigurationsAnnotation = "executable.usvc-dev.developer.microsoft.com/launch-configurations"

	// The following 3 annotations are deprecated and will be removed in future release
	csharpProjectPathAnnotation          = "csharp-project-path"
	csharpLaunchProfileAnnotation        = "csharp-launch-profile"
	csharpDisableLaunchProfileAnnotation = "csharp-disable-launch-profile"

	version20240303      = "2024-03-03"
	queryParamApiVersion = "api-version"

	defaultIdeEndpointRequestTimeout = 5 * time.Second
)
