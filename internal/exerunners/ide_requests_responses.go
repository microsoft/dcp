package exerunners

import (
	"fmt"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
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

type ideRunSessionChangeNotification struct {
	ideSessionNotificationBase
	PID      process.Pid_t `json:"pid,omitempty"`
	ExitCode *int32        `json:"exit_code,omitempty"`
}

type ideSessionLogNotification struct {
	ideSessionNotificationBase
	IsStdErr   bool   `json:"is_std_err"`
	LogMessage string `json:"log_message"`
}

func (scn *ideRunSessionChangeNotification) ToString() string {
	maybePID := ""
	if scn.PID != 0 {
		maybePID = fmt.Sprintf(" (PID: %d)", scn.PID)
	}
	retval := fmt.Sprintf("Session %s: %s%s", scn.SessionID, scn.NotificationType, maybePID)
	return retval
}

type ideRunSessionRequest struct {
	ProjectPath string         `json:"project_path"`
	Debug       bool           `json:"debug,omitempty"`
	Env         []apiv1.EnvVar `json:"env,omitempty"`
	Args        []string       `json:"args,omitempty"`
}

const (
	ideRunSessionResourcePath             = "/run_session"
	ideRunSessionNotificationResourcePath = "/run_session/notify"

	ideEndpointPortVar  = "DEBUG_SESSION_PORT"
	ideEndpointTokenVar = "DEBUG_SESSION_TOKEN"

	csharpProjectPathAnnotation = "csharp-project-path"
)
