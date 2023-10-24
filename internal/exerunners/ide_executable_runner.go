package exerunners

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/osutil"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	MaxParallelIdeExecutableStarts = 6
)

var lineSeparator []byte

func init() {
	if runtime.GOOS == "windows" {
		lineSeparator = []byte("\r\n")
	} else {
		lineSeparator = []byte("\n")
	}
}

type runState struct {
	runID         controllers.RunID
	sessionURL    string // The URL of the run session resource in the IDE
	changeHandler controllers.RunChangeHandler
	handlerWG     *sync.WaitGroup
	pid           process.Pid_t
	finished      bool
	exitCode      *int32
	stdOut        *usvc_io.BufferedWrappingWriter
	stdErr        *usvc_io.BufferedWrappingWriter
}

func NewRunState() *runState {
	rs := &runState{
		runID:         "",
		changeHandler: nil,
		handlerWG:     &sync.WaitGroup{},
		finished:      false,
		exitCode:      apiv1.UnknownExitCode,
		stdOut:        usvc_io.NewBufferedWrappingWriter(),
		stdErr:        usvc_io.NewBufferedWrappingWriter(),
	}

	// Two things need to happen before the completion handler is called:
	// 1. The StartRun() method of the IDE runner must get a chance to set the completion handler.
	// 2. The IDE runner consumer must call startWaitForRunCompletion() for the run.
	rs.handlerWG.Add(2)

	return rs
}

func (rs *runState) NotifyRunChangedAsync() {
	go func() {
		rs.handlerWG.Wait()

		if rs.changeHandler != nil {
			rs.changeHandler.OnRunChanged(rs.runID, rs.pid, rs.exitCode, nil)
		}
	}()
}

func (rs *runState) IncreaseCompletionCallReadiness() {
	rs.handlerWG.Done()
}

func (rs *runState) SetOutputWriters(stdOut, stdErr io.Writer) error {
	var err error
	if stdOut != nil {
		err = rs.stdOut.SetTarget(stdOut)
	} else {
		err = rs.stdOut.SetTarget(io.Discard)
	}
	if err != nil {
		return err
	}

	if stdErr != nil {
		err = rs.stdErr.SetTarget(stdErr)
	} else {
		err = rs.stdErr.SetTarget(io.Discard)
	}
	if err != nil {
		return err
	}

	return nil
}

func (rs *runState) CloseOutputWriters() {
	rs.stdOut.Close()
	rs.stdErr.Close()
}

type IdeExecutableRunner struct {
	notifySocket *websocket.Conn
	lock         *sync.Mutex
	startupQueue *resiliency.WorkQueue // Queue for starting IDE run sessions
	startingRuns syncmap.Map[types.NamespacedName, bool]
	activeRuns   syncmap.Map[controllers.RunID, *runState]
	log          logr.Logger
	portStr      string // The local port on which the IDE is listening for run session requests
	tokenStr     string // The security token to use when connecting to the IDE
}

func NewIdeExecutableRunner(lifetimeCtx context.Context, log logr.Logger) (*IdeExecutableRunner, error) {
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
		lock:         &sync.Mutex{},
		startupQueue: resiliency.NewWorkQueue(lifetimeCtx, MaxParallelIdeExecutableStarts),
		startingRuns: syncmap.Map[types.NamespacedName, bool]{},
		activeRuns:   syncmap.Map[controllers.RunID, *runState]{},
		log:          log,
		portStr:      portStr,
		tokenStr:     tokenStr,
	}, nil
}

func (r *IdeExecutableRunner) StartRun(ctx context.Context, exe *apiv1.Executable, runChangeHandler controllers.RunChangeHandler, log logr.Logger) error {
	const runSessionCouldNotBeStarted = "run session could not be started: "

	err := r.ensureNotificationSocket()
	if err != nil {
		return fmt.Errorf(runSessionCouldNotBeStarted+"%w", err)
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	// If we've already started running, simply return
	if _, found := r.startingRuns.Load(exe.NamespacedName()); found {
		return nil
	}

	exeStatus := exe.Status.DeepCopy()
	namespacedName := exe.NamespacedName()

	err = r.startupQueue.Enqueue(func(_ context.Context) {
		// Marking the Executable as "failed to start" will end its lifecycle.
		// We do this when we encounter an error that is unlikely to be resolved by retrying.
		markAsFailedToStart := func(exe *apiv1.Executable) {
			exeStatus.FinishTimestamp = metav1.Now()
			exeStatus.State = apiv1.ExecutableStateFailedToStart
			r.lock.Lock()
			defer r.lock.Unlock()

			r.startingRuns.Store(exe.NamespacedName(), true)
			runChangeHandler.OnStarted(namespacedName, controllers.UnknownRunID, *exeStatus, func() {})
		}

		markForRetry := func(exe *apiv1.Executable) {
			r.lock.Lock()
			defer r.lock.Unlock()

			r.startingRuns.Delete(exe.NamespacedName())
			runChangeHandler.OnStarted(namespacedName, controllers.UnknownRunID, *exeStatus, func() {})
		}

		req, err := r.prepareRunRequest(exe)
		if err != nil {
			log.Error(err, "run session could not be started")
			markAsFailedToStart(exe)
			return
		}

		// Set up temp files for capturing stdout and stderr. These files (if successfully created)
		// will be cleaned up by the Executable controller when the Executable is deleted.

		stdOutFile, err := usvc_io.OpenFile(filepath.Join(os.TempDir(), fmt.Sprintf("%s_out_%s", exe.Name, exe.UID)), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
		if err != nil {
			log.Error(err, "failed to create temporary file for capturing standard output data")
			stdOutFile = nil
		} else {
			exe.Status.StdOutFile = stdOutFile.Name()
		}

		stdErrFile, err := usvc_io.OpenFile(filepath.Join(os.TempDir(), fmt.Sprintf("%s_err_%s", exe.Name, exe.UID)), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
		if err != nil {
			log.Error(err, "failed to create temporary file for capturing standard error data")
			stdErrFile = nil
		} else {
			exe.Status.StdErrFile = stdErrFile.Name()
		}

		if rawRequest, err := httputil.DumpRequest(req, true); err == nil {
			r.log.V(1).Info("Sending IDE run session request", "Request", string(rawRequest))
		} else {
			r.log.V(1).Info("Sending IDE run session request", "URL", req.URL)
		}

		client := http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			r.log.Error(err, "run session could not be started")
			markForRetry(exe)
			return
		}
		defer resp.Body.Close()

		if rawResponse, err := httputil.DumpResponse(resp, true); err == nil {
			r.log.V(1).Info("Completed IDE run session request", "Response", string(rawResponse))
		} else {
			r.log.V(1).Info("Completed IDE run session request", "URL", req.URL)
		}

		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body) // Best effort to get as much data about the error as possible
			log.Error(err, "run session could not be started", "Status", resp.Status, "Body", respBody)
			markAsFailedToStart(exe) // The IDE refused to run this Executable
			return
		}

		sessionURL := resp.Header.Get("Location")
		if sessionURL == "" {
			markForRetry(exe)
			return
		}
		rid, err := getLastUrlPathSegment(sessionURL)
		if err != nil {
			markForRetry(exe)
		}

		runID := controllers.RunID(rid)
		r.log.Info("IDE run session started", "RunID", runID)
		exeStatus.ExecutionID = string(runID)
		exeStatus.StartupTimestamp = metav1.Now()

		startWaitForRunCompletion := func() {}

		// Take the lock so we can manipulate the run state safely.
		r.lock.Lock()
		defer r.lock.Unlock()

		rs := r.ensureRunState(runID)
		rs.sessionURL = sessionURL
		defer rs.IncreaseCompletionCallReadiness()
		err = rs.SetOutputWriters(stdOutFile, stdErrFile)
		if err != nil {
			log.Error(err, "failed to set output writers to capture stdout/stderr") // Should never happen
		}

		if runChangeHandler != nil {
			rs.changeHandler = runChangeHandler
			startWaitForRunCompletion = func() {
				rs.IncreaseCompletionCallReadiness()
			}
			if rs.finished {
				r.activeRuns.Delete(runID)
				exeStatus.State = apiv1.ExecutableStateFinished
				exeStatus.FinishTimestamp = metav1.Now()
				runChangeHandler.OnStarted(namespacedName, runID, *exeStatus, startWaitForRunCompletion)
				return
			}
		}

		exeStatus.State = apiv1.ExecutableStateRunning
		runChangeHandler.OnStarted(namespacedName, runID, *exeStatus, startWaitForRunCompletion)
		r.startingRuns.Delete(namespacedName)
	})

	if err != nil {
		log.Error(err, "could not enqueue ide executable start operation")
		exe.Status.State = apiv1.ExecutableStateFailedToStart
		exe.Status.FinishTimestamp = metav1.Now()
	} else {
		exe.Status.State = apiv1.ExecutableStateStarting
		r.startingRuns.Store(namespacedName, true)
	}

	return nil
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

func (r *IdeExecutableRunner) prepareRunRequest(exe *apiv1.Executable) (*http.Request, error) {
	projectPath := exe.Annotations[csharpProjectPathAnnotation]
	if projectPath == "" {
		return nil, fmt.Errorf("missing required annotation '%s'", csharpProjectPathAnnotation)
	}

	isr := ideRunSessionRequest{
		ProjectPath: projectPath,
		Env:         exe.Status.EffectiveEnv,
		Args:        exe.Spec.Args,
	}
	isrBody, err := json.Marshal(isr)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%s%s", r.portStr, ideRunSessionResourcePath)

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewBuffer(isrBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.tokenStr))
	return req, nil
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
			var basicNotification ideSessionNotificationBase
			err = json.Unmarshal(msg, &basicNotification)
			if err != nil {
				r.log.Error(err, "invalid IDE basic session notification received, recycling connection...")
				r.closeNotifySocket()
				return
			}

			if basicNotification.SessionID == "" {
				r.log.Error(fmt.Errorf("received IDE run session notification with empty session ID"), "recycling connection...")
				r.closeNotifySocket()
				return
			}

			switch basicNotification.NotificationType {
			case notificationTypeProcessRestarted, notificationTypeSessionTerminated:
				var nsc ideRunSessionChangeNotification
				err = json.Unmarshal(msg, &nsc)
				if err != nil {
					r.log.Error(err, "invalid IDE run session notification received, recycling connection...")
					r.closeNotifySocket()
					return
				} else {
					r.handleSessionChange(nsc)
				}

			case notificationTypeServiceLogs:
				var nsl ideSessionLogNotification
				err = json.Unmarshal(msg, &nsl)
				if err != nil {
					r.log.Error(err, "invalid IDE run session notification received, recycling connection...")
					r.closeNotifySocket()
					return
				} else {
					r.handleSessionLogs(nsl)
				}
			}

		default:
			r.log.Info("unexpected message type '%c' received from session notification endpoint, ignoring...", msgType)
		}
	}
}

func (r *IdeExecutableRunner) handleSessionChange(scn ideRunSessionChangeNotification) {
	runID := controllers.RunID(scn.SessionID)
	exitCode := apiv1.UnknownExitCode
	if scn.ExitCode != nil {
		exitCode = new(int32)
		*exitCode = *scn.ExitCode
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	var runState *runState
	if exitCode != apiv1.UnknownExitCode {
		runState = r.fetchRunStateForDeletion(runID)
		runState.finished = true
		runState.CloseOutputWriters()
	} else {
		runState = r.ensureRunState(runID)
	}

	r.log.V(1).Info("IDE run session changed", "RunID", runID, "PID", scn.PID, "ExitCode", exitCode)

	runState.runID = runID
	runState.pid = scn.PID
	runState.exitCode = exitCode
	runState.NotifyRunChangedAsync()
}

func (r *IdeExecutableRunner) handleSessionLogs(nsl ideSessionLogNotification) {
	runID := controllers.RunID(nsl.SessionID)

	runState := r.ensureRunState(runID)
	var err error
	msg := withNewLine([]byte(nsl.LogMessage))
	if nsl.IsStdErr {
		_, err = runState.stdErr.Write(msg)
	} else {
		_, err = runState.stdOut.Write(msg)
	}

	if err != nil {
		r.log.Error(err, "failed to persist a log message")
	}
}

func (r *IdeExecutableRunner) ensureRunState(runID controllers.RunID) *runState {
	// Notifications for a particular run may arrive befeore the call to create a run session returns.
	// That is why we use LoadOrStoreNew in places where we want to ensure that we have a runState for a given run ID.
	rs, _ := r.activeRuns.LoadOrStoreNew(runID, func() *runState {
		return NewRunState()
	})
	return rs
}

func (r *IdeExecutableRunner) fetchRunStateForDeletion(runID controllers.RunID) *runState {
	runState, found := r.activeRuns.LoadAndDelete(runID)
	if !found {
		// If the project has issues, we might receive a very early notification about run session termination,
		// where that notification is the first piece of information we receive about the run session.
		// If that is the case we need to create a run state for the run session so that we can store the exit code and run status.
		// This means this run state will never be removed from activeRuns, but this should happen rearly and it does consume very little memory.
		// If this proves to be an issue, consider adding a TTL to the run state and scavenge on a timer.
		runState = NewRunState()
		r.activeRuns.Store(runID, runState)
	}
	return runState
}

func (r *IdeExecutableRunner) closeNotifySocket() {
	r.lock.Lock()
	if r.notifySocket != nil {
		_ = r.notifySocket.Close()
		r.notifySocket = nil
	}
	r.lock.Unlock()
}

func getLastUrlPathSegment(rawURL string) (string, error) {
	url, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	pathSegments := strings.Split(url.Path, "/")
	if len(pathSegments) == 0 {
		return "", fmt.Errorf("URL '%s' has no path segments", rawURL)
	}
	return pathSegments[len(pathSegments)-1], nil
}

func withNewLine(msg []byte) []byte {
	if endsWith(msg, lineSeparator) {
		return msg
	}
	return append(msg, lineSeparator...)
}

func endsWith(a, b []byte) bool {
	// If a is shorter than b, a doesn't end with b.
	if len(a) < len(b) {
		return false
	}

	// If any of the last len(b) characters of a don't match b, a doesn't end with b.
	start := len(a) - len(b)
	for i := range b {
		if a[start+i] != b[i] {
			return false
		}
	}

	return true
}

var _ controllers.ExecutableRunner = (*IdeExecutableRunner)(nil)
