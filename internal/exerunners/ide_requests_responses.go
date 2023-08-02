package exerunners

import (
	"fmt"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
)

type notificationType string

const (
	notificationTypeProcessRestarted  notificationType = "processRestarted"
	notificationTypeSessionTerminated notificationType = "sessionTerminated"
)

type ideRunSessionChangeNotification struct {
	NotificationType notificationType `json:"notification_type"`
	PID              uint16           `json:"pid,omitempty"`
	SessionID        string           `json:"session_id,omitempty"`
	ExitCode         *int32           `json:"exit_code,omitempty"`
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
