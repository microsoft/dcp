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

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"

	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
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
	lock                *sync.Mutex
	startupQueue        *resiliency.WorkQueue // Queue for starting IDE run sessions
	activeRuns          syncmap.Map[controllers.RunID, *runState]
	log                 logr.Logger
	lifetimeCtx         context.Context // Lifetime context of the controller hosting this runner
	connectionInfo      *ideConnectionInfo
	notificationHandler *ideNotificationHandler
}

func NewIdeExecutableRunner(lifetimeCtx context.Context, log logr.Logger) (*IdeExecutableRunner, error) {
	connInfo, err := NewIdeConnectionInfo(lifetimeCtx, log)
	if err != nil {
		return nil, err
	}

	r := &IdeExecutableRunner{
		lock:           &sync.Mutex{},
		startupQueue:   resiliency.NewWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency),
		activeRuns:     syncmap.Map[controllers.RunID, *runState]{},
		log:            log,
		lifetimeCtx:    lifetimeCtx,
		connectionInfo: connInfo,
	}

	nh := NewIdeNotificationHandler(lifetimeCtx, r, connInfo, log)
	r.notificationHandler = nh
	return r, nil
}

func (r *IdeExecutableRunner) StartRun(ctx context.Context, exe *apiv1.Executable, runChangeHandler controllers.RunChangeHandler, log logr.Logger) error {
	const runSessionCouldNotBeStarted = "run session could not be started: "

	if runChangeHandler == nil {
		return fmt.Errorf(runSessionCouldNotBeStarted + "missing required runChangeHandler")
	}

	ideConnErr := r.notificationHandler.WaitConnected(ctx)
	if ideConnErr != nil {
		return fmt.Errorf(runSessionCouldNotBeStarted+"%w", ideConnErr)
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

		req, reqCancel, err := r.prepareRunRequest(exe)
		if err != nil {
			log.Error(err, runSessionCouldNotBeStarted+"failed to prepare run session request")
			reportStartupFailure()
			return
		}
		defer reqCancel()

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

		var resp *http.Response
		resp, err = r.connectionInfo.GetClient().Do(req)
		if err != nil {
			log.Error(err, runSessionCouldNotBeStarted+"request round-trip failed")
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
			log.Error(err, runSessionCouldNotBeStarted+"IDE returned a response indicating failure",
				"Status", resp.Status,
				"Body", parseResponseBody(respBody),
			)
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
		log.V(1).Info("IDE run session started", "RunID", runID)
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

	req, reqCancel, err := r.makeRequest(ideRunSessionResourcePath+"/"+string(runID), http.MethodDelete, nil)
	if err != nil {
		return fmt.Errorf(runSessionCouldNotBeStopped+"failed to create request: %w", err)
	}
	defer reqCancel()

	resp, err := r.connectionInfo.GetClient().Do(req)
	if err != nil {
		return fmt.Errorf(runSessionCouldNotBeStopped+"%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		r.log.V(1).Info("IDE run session stopped", "RunID", runID)
		return nil
	}

	if resp.StatusCode == http.StatusNoContent {
		r.log.Info("Attempted to stop IDE run session which was not found", "RunID", runID)
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body) // Best effort to get as much data about the error as possible
	return fmt.Errorf(runSessionCouldNotBeStopped+"%s %s", resp.Status, parseResponseBody(respBody))
}

func (r *IdeExecutableRunner) prepareRunRequest(exe *apiv1.Executable) (*http.Request, context.CancelFunc, error) {
	var isrBody []byte
	var bodyErr error

	if equalOrNewer(r.connectionInfo.apiVersion, version20240303) {
		isrBody, bodyErr = r.prepareRunRequestV1(exe)
		if bodyErr != nil {
			return nil, nil, fmt.Errorf("failed to prepare IDE run request: %w", bodyErr)
		}
	} else {
		isrBody, bodyErr = r.prepareRunRequestDeprecated(exe)
		if bodyErr != nil {
			return nil, nil, fmt.Errorf("failed to prepare IDE run request: %w", bodyErr)
		}
	}

	req, reqCancel, reqErr := r.makeRequest(ideRunSessionResourcePath, http.MethodPut, bytes.NewBuffer(isrBody))
	if reqErr != nil {
		return nil, nil, fmt.Errorf("failed to create IDE run session request: %w", reqErr)
	}
	return req, reqCancel, nil
}

func (r *IdeExecutableRunner) prepareRunRequestDeprecated(exe *apiv1.Executable) ([]byte, error) {
	if exe.Annotations[csharpProjectPathAnnotation] != "" {
		projectPath := exe.Annotations[csharpProjectPathAnnotation]

		isr := ideRunSessionRequestDeprecated{
			ProjectPath: projectPath,
			Env:         exe.Status.EffectiveEnv,
			Args:        exe.Status.EffectiveArgs,
		}
		if _, disableLaunchProfile := exe.Annotations[csharpDisableLaunchProfileAnnotation]; disableLaunchProfile {
			isr.DisableLaunchProfile = true
		} else if launchProfile, haveLaunchProfile := exe.Annotations[csharpLaunchProfileAnnotation]; haveLaunchProfile {
			isr.LaunchProfile = launchProfile
		}

		isrBody, err := json.Marshal(isr)
		if err != nil {
			return nil, fmt.Errorf("failed to create Executable run request body: %w", err)
		}
		return isrBody, nil
	} else if exe.Annotations[launchConfigurationsAnnotation] != "" {
		// A bif of a transient situation, but possible in Aspire P5 timeframe: we have got the V1 annotation,
		// but we are asked to speak the deprecated protocol.

		var launchConfigs []projectLaunchConfiguration
		unmarshalErr := json.Unmarshal([]byte(exe.Annotations[launchConfigurationsAnnotation]), &launchConfigs)
		if unmarshalErr != nil {
			return nil, fmt.Errorf("Executable cannot be run because its launch configuration is invalid: %w", unmarshalErr)
		}
		if len(launchConfigs) != 1 {
			return nil, fmt.Errorf("Executable cannot be run; it must have exactly one launch configuration, but %d were provided", len(launchConfigs))
		}

		isr := ideRunSessionRequestDeprecated{
			ProjectPath:          launchConfigs[0].ProjectPath,
			Env:                  exe.Status.EffectiveEnv,
			Args:                 exe.Status.EffectiveArgs,
			LaunchProfile:        launchConfigs[0].LaunchProfile,
			DisableLaunchProfile: launchConfigs[0].DisableLaunchProfile,
			Debug:                launchConfigs[0].LaunchMode == projectLaunchModeDebug,
		}

		isrBody, err := json.Marshal(isr)
		if err != nil {
			return nil, fmt.Errorf("failed to create Executable run request body: %w", err)
		}
		return isrBody, nil
	} else {
		return nil, fmt.Errorf("IDE execution was requested for the Executable but the required '%s' annotation is missing", launchConfigurationsAnnotation)
	}
}

func (r *IdeExecutableRunner) prepareRunRequestV1(exe *apiv1.Executable) ([]byte, error) {
	if exe.Annotations[launchConfigurationsAnnotation] != "" {
		launchConfigsRaw := exe.Annotations[launchConfigurationsAnnotation]
		var launchConfigs json.RawMessage
		unmarshalErr := json.Unmarshal([]byte(launchConfigsRaw), &launchConfigs)
		if unmarshalErr != nil {
			return nil, fmt.Errorf("Executable cannot be run because its launch configuration is invalid: %w", unmarshalErr)
		}

		isr := ideRunSessionRequestV1{
			LaunchConfigurations: json.RawMessage(launchConfigs),
			Env:                  exe.Status.EffectiveEnv,
			Args:                 exe.Status.EffectiveArgs,
		}

		isrBody, marshalErr := json.Marshal(isr)
		if marshalErr != nil {
			return nil, fmt.Errorf("failed to create Executable run request body: %w", marshalErr)
		}
		return isrBody, nil
	} else if exe.Annotations[csharpProjectPathAnnotation] != "" {
		// A bif of a transient situation, but possible in Aspire P5 timeframe: we have got the old annotation,
		// but we are asked to speak the new protocol.

		isr := ideRunSessionRequestV1Explicit{
			LaunchConfigurations: []projectLaunchConfiguration{
				{
					ideLaunchConfiguration: ideLaunchConfiguration{Type: launchConfigurationTypeProject},
					ProjectPath:            exe.Annotations[csharpProjectPathAnnotation],
				},
			},
			Env:  exe.Status.EffectiveEnv,
			Args: exe.Status.EffectiveArgs,
		}

		if _, disableLaunchProfile := exe.Annotations[csharpDisableLaunchProfileAnnotation]; disableLaunchProfile {
			isr.LaunchConfigurations[0].DisableLaunchProfile = true
		} else if launchProfile, haveLaunchProfile := exe.Annotations[csharpLaunchProfileAnnotation]; haveLaunchProfile {
			isr.LaunchConfigurations[0].LaunchProfile = launchProfile
		}

		isrBody, marshalErr := json.Marshal(isr)
		if marshalErr != nil {
			return nil, fmt.Errorf("failed to create Executable run request body: %w", marshalErr)
		}
		return isrBody, nil
	} else {
		return nil, fmt.Errorf("IDE execution was requested for the Executable but the required '%s' annotation is missing",
			launchConfigurationsAnnotation)
	}
}

func (r *IdeExecutableRunner) HandleSessionChange(pcn ideRunSessionProcessChangedNotification) {
	runID := controllers.RunID(pcn.SessionID)
	r.log.V(1).Info("IDE run session changed", "RunID", runID, "PID", pcn.PID)

	r.lock.Lock()
	defer r.lock.Unlock()

	runState := r.ensureRunState(runID)
	runState.runID = runID
	runState.pid = pcn.PID

	runState.NotifyRunChangedAsync(r.lock)
}

func (r *IdeExecutableRunner) HandleSessionTermination(stn ideRunSessionTerminatedNotification) {
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

func (r *IdeExecutableRunner) HandleServiceLogs(nsl ideSessionLogNotification) {
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

func (r *IdeExecutableRunner) makeRequest(
	requestPath string,
	httpMethod string,
	body io.Reader,
) (*http.Request, context.CancelFunc, error) {
	return r.connectionInfo.MakeIdeRequest(
		r.lifetimeCtx,
		requestPath,
		httpMethod,
		body,
	)
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

var _ controllers.ExecutableRunner = (*IdeExecutableRunner)(nil)
