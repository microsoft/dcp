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
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type runState struct {
	runID             controllers.RunID
	completionHandler controllers.RunCompletionHandler
	handlerWG         *sync.WaitGroup
	finished          bool
	exitCode          int32
}

func NewRunState() *runState {
	rs := &runState{
		runID:             "",
		completionHandler: nil,
		handlerWG:         &sync.WaitGroup{},
		finished:          false,
		exitCode:          apiv1.UnknownExitCode,
	}

	// Three things need to happen before the completion handler is called:
	// 1. The StartRun() method of the IDE runner must get a chance to set the completion handler.
	// 2. The run session notification must be received from the IDE (with the exit code).
	// 3. The IDE runner consumer must call startWaitForRunCompletion() for the run.
	rs.handlerWG.Add(3)

	return rs
}

func (rs *runState) NotifyRunCompletedAsync() {
	go func() {
		rs.handlerWG.Wait()

		if rs.completionHandler != nil {
			rs.completionHandler.OnRunCompleted(rs.runID, rs.exitCode, nil)
		}
	}()
}

func (rs *runState) IncreaseCompletionCallReadiness() {
	rs.handlerWG.Done()
}

type IdeExecutableRunner struct {
	notifySocket *websocket.Conn
	lock         *sync.Mutex
	activeRuns   syncmap.Map[controllers.RunID, *runState]
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
		activeRuns: syncmap.Map[controllers.RunID, *runState]{},
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

	// Start with orchestrator process environemnt, then override with Executable environment.
	envMap := maps.SliceToMap(os.Environ(), func(envStr string) (string, string) {
		parts := strings.SplitN(envStr, "=", 2)
		return parts[0], parts[1]
	})
	for _, envVar := range exe.Spec.Env {
		envMap[envVar.Name] = envVar.Value
	}
	effectiveEnv := maps.MapToSlice[string, string, apiv1.EnvVar](envMap, func(key, value string) apiv1.EnvVar {
		return apiv1.EnvVar{Name: key, Value: value}
	})

	isr := ideRunSessionRequest{
		ProjectPath: projectPath,
		Env:         effectiveEnv,
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
		return "", nil, fmt.Errorf(runSessionCouldNotBeStarted + "the returned run ID was empty")
	}

	r.log.Info("IDE run session started", "RunID", runID)
	exe.Status.ExecutionID = string(runID)
	exe.Status.StartupTimestamp = metav1.Now()

	startWaitForRunCompletion := func() {}
	// We might receive notifications for this run before we have a chance to create the run state instance here.
	// That is why we use LoadOrStoreNew instead of just creating a new instance of runState.
	rs, _ := r.activeRuns.LoadOrStoreNew(runID, func() *runState {
		return NewRunState()
	})
	defer rs.IncreaseCompletionCallReadiness()

	if runCompletionHandler != nil {
		r.lock.Lock()
		defer r.lock.Unlock()

		rs.completionHandler = runCompletionHandler
		startWaitForRunCompletion = func() {
			rs.IncreaseCompletionCallReadiness()
		}
		if rs.finished {
			r.activeRuns.Delete(runID)
			exe.Status.State = apiv1.ExecutableStateFinished
			exe.Status.FinishTimestamp = metav1.Now()
			return runID, startWaitForRunCompletion, nil
		}
	}

	exe.Status.State = apiv1.ExecutableStateRunning
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
		exitCode := int32(apiv1.UnknownExitCode)
		if scn.ExitCode != nil {
			exitCode = *scn.ExitCode
		}

		r.lock.Lock()
		defer r.lock.Unlock()

		runState, found := r.activeRuns.LoadAndDelete(runID)
		if !found {
			// This happens if we receive an early notification for run session that has just started.
			// Store the run status and exit code. The run completion handler will be filled by the StartRun method.
			runState = NewRunState()
			r.activeRuns.Store(runID, runState)
		}

		runState.finished = true
		runState.exitCode = exitCode
		runState.IncreaseCompletionCallReadiness()
		runState.NotifyRunCompletedAsync()

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
