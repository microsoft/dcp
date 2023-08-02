package exerunners

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/pkg/randdata"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type runState struct {
	completionHandler controllers.RunCompletionHandler
	handlerLock       *sync.Mutex
}

type IdeExecutableRunner struct {
	notifySocket *websocket.Conn
	lock         *sync.Mutex
	activeRuns   syncmap.Map[controllers.RunID, runState]
	log          logr.Logger
	portStr      string // The local port on which the IDE is listening for run session requests
	tokenStr     string // The security token to use when connecting to the IDE
}

func NewIdeExecutableRunner(log logr.Logger) (*IdeExecutableRunner, error) {
	const runnerNotAvailable = "Executables cannot be started via IDE: "
	const missingRequiredEnvVar = "missing required environment variable '%s'"

	portStr, found := os.LookupEnv(ideEndpointPortVar)
	if !found {
		err := fmt.Errorf(missingRequiredEnvVar, ideEndpointPortVar)
		log.Info(runnerNotAvailable + err.Error())
		return nil, err
	}
	portStr = strings.TrimSpace(portStr)
	portStr = strings.TrimPrefix(portStr, "localhost:") // Visual Studio prepends "localhost:" to port number.

	tokenStr, found := os.LookupEnv(ideEndpointTokenVar)
	if !found {
		err := fmt.Errorf(missingRequiredEnvVar, ideEndpointTokenVar)
		log.Info(runnerNotAvailable + err.Error())
		return nil, err
	}
	tokenStr = strings.TrimSpace(tokenStr)

	return &IdeExecutableRunner{
		lock:       &sync.Mutex{},
		activeRuns: syncmap.Map[controllers.RunID, runState]{},
		log:        log,
		portStr:    portStr,
		tokenStr:   tokenStr,
	}, nil
}

func (r *IdeExecutableRunner) StartRun(ctx context.Context, exe *apiv1.Executable, runCompletionHandler controllers.RunCompletionHandler, log logr.Logger) (controllers.RunID, func(), error) {
	const runSessionCouldNotBeStarted = "run session could not be started: "

	// Marking the Executable as "failed to start" will end its lifecycle.
	// We do this when we encounter an error that is unlikely to be resolved by retrying.
	markAsFailedToStart := func(exe *apiv1.Executable) {
		exe.Status.FinishTimestamp = metav1.Now()
		exe.Status.State = apiv1.ExecutableStateFailedToStart
	}

	err := r.ensureNotificationSocket()
	if err != nil {
		return "", nil, fmt.Errorf(runSessionCouldNotBeStarted+"%w", err)
	}

	projectPath := exe.Annotations[csharpProjectPathAnnotation]
	if projectPath == "" {
		markAsFailedToStart(exe)
		return "", nil, fmt.Errorf(runSessionCouldNotBeStarted+"missing required annotation '%s'", csharpProjectPathAnnotation)
	}
	isr := ideRunSessionRequest{
		ProjectPath: projectPath,
		Env:         exe.Spec.Env,
		Args:        exe.Spec.Args,
	}
	isrBody, err := json.Marshal(isr)
	if err != nil {
		markAsFailedToStart(exe)
		return "", nil, fmt.Errorf(runSessionCouldNotBeStarted+"failed to marshal request body: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%s%s", r.portStr, ideRunSessionResourcePath)

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer(isrBody))
	if err != nil {
		return "", nil, fmt.Errorf(runSessionCouldNotBeStarted+"failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.tokenStr))

	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf(runSessionCouldNotBeStarted+"%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		markAsFailedToStart(exe)             // The IDE refused to run this Executable
		respBody, _ := io.ReadAll(resp.Body) // Best effort to get as much data about the error as possible
		return "", nil, fmt.Errorf(runSessionCouldNotBeStarted+"%s %s", resp.Status, respBody)
	}

	var runID controllers.RunID = controllers.RunID(resp.Header.Get("Location"))
	if runID == "" {
		// HACK tis is a workaround for a bug in the IDE where it does not return the run ID in the Location header
		randStr, _ := randdata.MakeRandomString(8)
		runID = controllers.RunID(randStr)
		// return "", nil, fmt.Errorf(runSessionCouldNotBeStarted + "the returned run ID was empty")
	}

	r.log.Info("IDE run session started", "RunID", runID)

	startWaitForRunCompletion := func() {}
	if runCompletionHandler != nil {
		rs := runState{
			completionHandler: runCompletionHandler,
			handlerLock:       &sync.Mutex{},
		}
		rs.handlerLock.Lock()
		startWaitForRunCompletion = func() {
			rs.handlerLock.Unlock() // It is OK to unlock a mutex from a different goroutine
		}
	}
	exe.Status.ExecutionID = string(runID)
	exe.Status.State = apiv1.ExecutableStateRunning
	exe.Status.StartupTimestamp = metav1.Now()

	return runID, startWaitForRunCompletion, nil
}

func (r *IdeExecutableRunner) StopRun(ctx context.Context, runID controllers.RunID, log logr.Logger) error {
	const runSessionCouldNotBeStopped = "run session could not be stopped: "

	url := fmt.Sprintf("http://localhost:%s%s/%s", r.portStr, ideRunSessionResourcePath, runID)

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf(runSessionCouldNotBeStopped+"failed to create request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.tokenStr))

	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf(runSessionCouldNotBeStopped+"%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		r.log.Info("IDE run session stopped", "RunID", runID)
		return nil
	}

	if resp.StatusCode == http.StatusNoContent {
		r.log.Info("Attempted to stop IDE run session which was not found", "RunID", runID)
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body) // Best effort to get as much data about the error as possible
	return fmt.Errorf(runSessionCouldNotBeStopped+"%s %s", resp.Status, respBody)
}

func (r *IdeExecutableRunner) ensureNotificationSocket() error {
	r.lock.Lock()
	if r.notifySocket != nil {
		r.lock.Unlock()
		return nil
	}
	r.lock.Unlock()

	headers := http.Header{}
	headers.Add("Authorization", fmt.Sprintf("Bearer %s", r.tokenStr))
	url := fmt.Sprintf("ws://localhost:%s%s", r.portStr, ideRunSessionNotificationResourcePath)

	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 5 * time.Second, // If it takes more than 5 seconds to connect, something is wrong
	}
	socket, _, err := dialer.Dial(url, headers)
	if err != nil {
		return fmt.Errorf("failed to connect to IDE run session notification endpoint: %w", err)
	}

	r.lock.Lock()
	if r.notifySocket == nil {
		r.notifySocket = socket
		go r.processNotifications()
	}
	r.lock.Unlock()
	return nil
}

func (r *IdeExecutableRunner) processNotifications() {
	for {
		msgType, msg, err := r.notifySocket.ReadMessage()
		if err != nil {
			r.closeNotifySocket()
			r.log.Error(err, "failed to read message from IDE run session notification endpoint, stopping notifications processing")
			return
		}

		switch msgType {
		case websocket.CloseMessage:
			r.log.Info("IDE closed the run session notification endpoint")
			r.closeNotifySocket()
			return

		case websocket.TextMessage:
			var scn ideRunSessionChangeNotification
			err = json.Unmarshal(msg, &scn)
			if err != nil {
				r.log.Error(err, "invalid IDE run session notification received, recycling connection...")
				r.closeNotifySocket()
				return
			}

			if scn.SessionID == "" {
				r.log.Error(fmt.Errorf("received IDE run session notification with empty session ID"), "recycling connection...")
				r.closeNotifySocket()
				return
			}

			r.handleSessionNotification(scn)

		default:
			r.log.Info("unexpected message type '%c' received from session notification endpoint, ignoring...", msgType)
		}
	}
}

func (r *IdeExecutableRunner) handleSessionNotification(scn ideRunSessionChangeNotification) {
	switch scn.NotificationType {
	case notificationTypeProcessRestarted:
		// We currently do not track process restarts, so we can ignore this notification
		return

	case notificationTypeSessionTerminated:
		runID := controllers.RunID(scn.SessionID)
		runState, found := r.activeRuns.LoadAndDelete(runID)
		if !found {
			// This can happen if StartRun() was called without a completion handler, in which case we do not track the run.
			// Also, it may be a notification for a run session that this orchestrator instance did not start.
			// Either way, we can ignore this notification.
			return
		}

		exitCode := int32(apiv1.UnknownExitCode)
		if scn.ExitCode != nil {
			exitCode = *scn.ExitCode
		}

		// Call the completion handler asynchronously so that the handling of other notifications is not blocked.
		go func() {
			runState.handlerLock.Lock()
			defer runState.handlerLock.Unlock()
			runState.completionHandler.OnRunCompleted(runID, exitCode, nil)
		}()

	default:
		r.log.Error(fmt.Errorf("received unexpected IDE run session notification type '%s'", scn.NotificationType), "ignoring...")
	}
}

func (r *IdeExecutableRunner) closeNotifySocket() {
	r.lock.Lock()
	if r.notifySocket != nil {
		_ = r.notifySocket.Close()
		r.notifySocket = nil
	}
	r.lock.Unlock()
}

var _ controllers.ExecutableRunner = (*IdeExecutableRunner)(nil)
