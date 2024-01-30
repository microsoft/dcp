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
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type startingState struct {
	status  apiv1.ExecutableStatus
	updated bool
}

func NewStartingState(status apiv1.ExecutableStatus) *startingState {
	return &startingState{
		status:  status,
		updated: false,
	}
}

type runState struct {
	name          types.NamespacedName
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

// Note: the order in which NotifyRunChangedAsync() are executed does not really matter, because that method
// always uses the latest data stored in the run state, and the consistency of the data is guaranteed by the lock.
// So we can safely "fire and forget" this method.
func (rs *runState) NotifyRunChangedAsync(locker sync.Locker) {
	go func() {
		rs.handlerWG.Wait()

		// Make sure the run state is not changed while we are gathering data for the change handler.
		locker.Lock()
		runID := rs.runID
		pid := rs.pid
		exitCode := apiv1.UnknownExitCode
		if rs.exitCode != apiv1.UnknownExitCode {
			exitCode = new(int32)
			*exitCode = *rs.exitCode
		}
		locker.Unlock()

		rs.changeHandler.OnRunChanged(runID, pid, exitCode, nil)
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
		startupQueue: resiliency.NewWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency),
		activeRuns:   syncmap.Map[controllers.RunID, *runState]{},
		log:          log,
		portStr:      portStr,
		tokenStr:     tokenStr,
	}, nil
}

func (r *IdeExecutableRunner) StartRun(ctx context.Context, exe *apiv1.Executable, runChangeHandler controllers.RunChangeHandler, log logr.Logger) error {
	const runSessionCouldNotBeStarted = "run session could not be started: "

	if runChangeHandler == nil {
		return fmt.Errorf(runSessionCouldNotBeStarted + "missing required runChangeHandler")
	}

	notifySocketErr := r.ensureNotificationSocket()
	if notifySocketErr != nil {
		return fmt.Errorf(runSessionCouldNotBeStarted+"%w", notifySocketErr)
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	namespacedName := exe.NamespacedName()

	exeStatus := exe.Status.DeepCopy()

	workEnqueueErr := r.startupQueue.Enqueue(func(_ context.Context) {
		var stdOutFile, stdErrFile *os.File

		reportStartupFailure := func() {
			r.lock.Lock()
			defer r.lock.Unlock()

			if stdOutFile != nil {
				_ = stdOutFile.Close()
				_ = os.Remove(exeStatus.StdOutFile)
				exeStatus.StdOutFile = ""
			}
			if stdErrFile != nil {
				_ = stdErrFile.Close()
				_ = os.Remove(exeStatus.StdErrFile)
				exeStatus.StdErrFile = ""
			}

			// Using UnknownRunID will cause the controller to assume that the Executable startup failed.
			runChangeHandler.OnStartingCompleted(namespacedName, controllers.UnknownRunID, *exeStatus, func() {})
		}

		req, err := r.prepareRunRequest(exe)
		if err != nil {
			log.Error(err, "run session could not be started")
			reportStartupFailure()
			return
		}

		// Set up temp files for capturing stdout and stderr. These files (if successfully created)
		// will be cleaned up by the Executable controller when the Executable is deleted.

		outputRootFolder := os.TempDir()
		if dcpSessionDir, found := os.LookupEnv(logger.DCP_SESSION_FOLDER); found {
			outputRootFolder = dcpSessionDir
		}

		stdOutFile, err = usvc_io.OpenFile(filepath.Join(outputRootFolder, fmt.Sprintf("%s_out_%s", exe.Name, exe.UID)), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
		if err != nil {
			log.Error(err, "failed to create temporary file for capturing standard output data")
			stdOutFile = nil
		} else {
			exeStatus.StdOutFile = stdOutFile.Name()
		}

		stdErrFile, err = usvc_io.OpenFile(filepath.Join(outputRootFolder, fmt.Sprintf("%s_err_%s", exe.Name, exe.UID)), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
		if err != nil {
			log.Error(err, "failed to create temporary file for capturing standard error data")
			stdErrFile = nil
		} else {
			exeStatus.StdErrFile = stdErrFile.Name()
		}

		if rawRequest, dumpRequestErr := httputil.DumpRequest(req, true); dumpRequestErr == nil {
			log.V(1).Info("Sending IDE run session request", "Request", string(rawRequest))
		} else {
			log.V(1).Info("Sending IDE run session request", "URL", req.URL)
		}

		client := http.Client{}
		var resp *http.Response
		resp, err = client.Do(req)
		if err != nil {
			log.Error(err, "run session could not be started")
			reportStartupFailure()
			return
		}
		defer resp.Body.Close()

		if rawResponse, dumpResponseErr := httputil.DumpResponse(resp, true); dumpResponseErr == nil {
			log.V(1).Info("Completed IDE run session request", "Response", string(rawResponse))
		} else {
			log.V(1).Info("Completed IDE run session request", "URL", req.URL)
		}

		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body) // Best effort to get as much data about the error as possible
			log.Error(err, "run session could not be started", "Status", resp.Status, "Body", respBody)
			reportStartupFailure() // The IDE refused to run this Executable
			return
		}

		sessionURL := resp.Header.Get("Location")
		if sessionURL == "" {
			reportStartupFailure()
			return
		}
		var rid string
		rid, err = getLastUrlPathSegment(sessionURL)
		if err != nil {
			reportStartupFailure()
		}

		runID := controllers.RunID(rid)
		log.Info("IDE run session started", "RunID", runID)
		exeStatus.ExecutionID = string(runID)
		exeStatus.StartupTimestamp = metav1.Now()

		r.lock.Lock()
		defer r.lock.Unlock()

		rs := r.ensureRunState(runID)
		rs.name = namespacedName
		rs.sessionURL = sessionURL
		defer rs.IncreaseCompletionCallReadiness()
		err = rs.SetOutputWriters(stdOutFile, stdErrFile)
		if err != nil {
			log.Error(err, "failed to set output writers to capture stdout/stderr") // Should never happen
		}

		rs.changeHandler = runChangeHandler
		startWaitForRunCompletion := func() {
			rs.IncreaseCompletionCallReadiness()
		}

		if rs.finished {
			r.activeRuns.Delete(runID)
			exeStatus.FinishTimestamp = metav1.Now()
			exeStatus.State = apiv1.ExecutableStateFinished
		} else {
			exeStatus.State = apiv1.ExecutableStateRunning
		}

		runChangeHandler.OnStartingCompleted(namespacedName, runID, *exeStatus, startWaitForRunCompletion)
	})

	if workEnqueueErr != nil {
		log.Error(workEnqueueErr, "could not enqueue ide executable start operation; workload is shutting down")
		exe.Status.State = apiv1.ExecutableStateFailedToStart
		exe.Status.FinishTimestamp = metav1.Now()
	} else {
		log.V(1).Info("Executable is starting, waiting for OnStarted callback...")
		exe.Status.State = apiv1.ExecutableStateStarting
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
		Args:        exe.Status.EffectiveArgs,
	}
	if launchProfile, found := exe.Annotations[csharpLaunchProfileAnnotation]; found {
		isr.LaunchProfile = launchProfile
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
			case notificationTypeProcessRestarted:
				var pcn ideRunSessionProcessChangedNotification
				err = json.Unmarshal(msg, &pcn)
				if err != nil {
					r.log.Error(err, "invalid IDE run session notification received, recycling connection...")
					r.closeNotifySocket()
					return
				} else {
					r.handleSessionChange(pcn)
				}

			case notificationTypeSessionTerminated:
				var stn ideRunSessionTerminatedNotification
				err = json.Unmarshal(msg, &stn)
				if err != nil {
					r.log.Error(err, "invalid IDE run session notification received, recycling connection...")
					r.closeNotifySocket()
					return
				} else {
					r.handleSessionTerminated(stn)
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

func (r *IdeExecutableRunner) handleSessionChange(pcn ideRunSessionProcessChangedNotification) {
	runID := controllers.RunID(pcn.SessionID)
	r.log.V(1).Info("IDE run session changed", "RunID", runID, "PID", pcn.PID)

	r.lock.Lock()
	defer r.lock.Unlock()

	runState := r.ensureRunState(runID)
	runState.runID = runID
	runState.pid = pcn.PID

	runState.NotifyRunChangedAsync(r.lock)
}

func (r *IdeExecutableRunner) handleSessionTerminated(stn ideRunSessionTerminatedNotification) {
	runID := controllers.RunID(stn.SessionID)
	exitCode := apiv1.UnknownExitCode
	if stn.ExitCode != nil {
		exitCode = new(int32)
		*exitCode = *stn.ExitCode
	}
	r.log.V(1).Info("IDE run session terminated", "RunID", runID, "PID", stn.PID, "ExitCode", exitCode)

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

	runState.runID = runID
	runState.pid = stn.PID
	runState.exitCode = exitCode
	runState.NotifyRunChangedAsync(r.lock)
}

func (r *IdeExecutableRunner) handleSessionLogs(nsl ideSessionLogNotification) {
	runID := controllers.RunID(nsl.SessionID)

	runState := r.ensureRunState(runID)
	var err error
	msg := osutil.WithNewline([]byte(nsl.LogMessage))
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
		// This means this run state will never be removed from activeRuns, but this should happen rarely and it does consume very little memory.
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

var _ controllers.ExecutableRunner = (*IdeExecutableRunner)(nil)
