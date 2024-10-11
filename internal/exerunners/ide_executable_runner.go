package exerunners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"

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

type IdeExecutableRunner struct {
	lock                *sync.Mutex
	startupQueue        *resiliency.WorkQueue // Queue for starting IDE run sessions
	activeRuns          *syncmap.Map[controllers.RunID, *runState]
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
		activeRuns:     &syncmap.Map[controllers.RunID, *runState]{},
		log:            log,
		lifetimeCtx:    lifetimeCtx,
		connectionInfo: connInfo,
	}

	nh := NewIdeNotificationHandler(lifetimeCtx, r, connInfo, log)
	r.notificationHandler = nh
	return r, nil
}

func (r *IdeExecutableRunner) StartRun(ctx context.Context, exe *apiv1.Executable, runInfo *controllers.ExecutableRunInfo, runChangeHandler controllers.RunChangeHandler, log logr.Logger) error {
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

	namespacedName := runInfo.NamespacedName()

	workEnqueueErr := r.startupQueue.Enqueue(func(_ context.Context) {
		var stdOutFile, stdErrFile *os.File

		// Lock the run info to prevent changes while we are starting the run session.
		runInfo.Lock()
		defer runInfo.Unlock()

		reportStartupFailure := func() {
			r.lock.Lock()
			defer r.lock.Unlock()

			if stdOutFile != nil {
				_ = stdOutFile.Close()
				_ = os.Remove(runInfo.StdOutFile)
				runInfo.StdOutFile = ""
			}
			if stdErrFile != nil {
				_ = stdErrFile.Close()
				_ = os.Remove(runInfo.StdErrFile)
				runInfo.StdErrFile = ""
			}

			// Using UnknownRunID will cause the controller to assume that the Executable startup failed.
			runChangeHandler.OnStartingCompleted(namespacedName, controllers.UnknownRunID, runInfo, func() {})
		}

		req, reqCancel, err := r.prepareRunRequest(exe, runInfo)
		if err != nil {
			log.Error(err, runSessionCouldNotBeStarted+"failed to prepare run session request")
			reportStartupFailure()
			return
		}
		defer reqCancel()

		// Set up temp files for capturing stdout and stderr. These files (if successfully created)
		// will be cleaned up by the Executable controller when the Executable is deleted.

		stdOutFile, err = usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
		if err != nil {
			log.Error(err, "failed to create temporary file for capturing standard output data")
			stdOutFile = nil
		} else {
			runInfo.StdOutFile = stdOutFile.Name()
		}

		stdErrFile, err = usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
		if err != nil {
			log.Error(err, "failed to create temporary file for capturing standard error data")
			stdErrFile = nil
		} else {
			runInfo.StdErrFile = stdErrFile.Name()
		}

		if rawRequest, dumpRequestErr := httputil.DumpRequest(req, true); dumpRequestErr == nil {
			log.V(1).Info("Sending IDE run session request", "Request", string(rawRequest))
		} else {
			log.V(1).Info("Sending IDE run session request", "URL", req.URL)
		}

		var resp *http.Response
		resp, err = r.connectionInfo.GetClient().Do(req)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				log.Error(err, fmt.Sprintf("timeout of %.0f seconds exceeded waiting for the IDE to start a run session; you can set the DCP_IDE_REQUEST_TIMEOUT_SECONDS environment variable to override this timeout (in seconds)", defaultIdeEndpointRequestTimeout.Seconds()))
			} else {
				log.Error(err, runSessionCouldNotBeStarted+"request round-trip failed")
			}

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
		runInfo.ExecutionID = string(runID)
		runInfo.StartupTimestamp = metav1.NowMicro()

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
			runInfo.SetState(apiv1.ExecutableStateFinished)
		} else {
			runInfo.SetState(apiv1.ExecutableStateRunning)
		}

		runChangeHandler.OnStartingCompleted(namespacedName, runID, runInfo, startWaitForRunCompletion)
	})

	if workEnqueueErr != nil {
		log.Error(workEnqueueErr, "could not enqueue ide executable start operation; workload is shutting down")
		runInfo.SetState(apiv1.ExecutableStateFailedToStart)
	} else {
		log.V(1).Info("Executable is starting, waiting for OnStarted callback...")
		runInfo.SetState(apiv1.ExecutableStateStarting)
	}

	return nil
}

func (r *IdeExecutableRunner) StopRun(ctx context.Context, runID controllers.RunID, log logr.Logger) error {
	const runSessionCouldNotBeStopped = "run session could not be stopped: "

	runState, found := r.activeRuns.Load(runID)
	if !found {
		r.log.Info("Attempted to stop IDE run session which was not found", "RunID", runID)
		return nil
	}

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
		// Wrap in a function to make it easier to ensure context cancel is called
		return func() error {
			// Wait up to 10 seconds for the IDE to send confirmation that the run session has been terminated (and stdio closed)
			ideExitCtx, ideExitCancel := context.WithTimeout(ctx, 10*time.Second)
			defer ideExitCancel()
			select {
			case <-ideExitCtx.Done():
				return fmt.Errorf("timeout waiting for IDE to confirm run session termination: %w", ideExitCtx.Err())
			case <-runState.exitCh:
				return nil
			}
		}()
	}

	if resp.StatusCode == http.StatusNoContent {
		r.log.Info("Attempted to stop IDE run session which was not found", "RunID", runID)
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body) // Best effort to get as much data about the error as possible
	return fmt.Errorf(runSessionCouldNotBeStopped+"%s %s", resp.Status, parseResponseBody(respBody))
}

func (r *IdeExecutableRunner) prepareRunRequest(exe *apiv1.Executable, runInfo *controllers.ExecutableRunInfo) (*http.Request, context.CancelFunc, error) {
	var isrBody []byte
	var bodyErr error

	if equalOrNewer(r.connectionInfo.apiVersion, version20240303) {
		isrBody, bodyErr = r.prepareRunRequestV1(exe, runInfo)
		if bodyErr != nil {
			return nil, nil, fmt.Errorf("failed to prepare IDE run request: %w", bodyErr)
		}
	} else {
		return nil, nil, fmt.Errorf("Aspire IDE extension is older than the minimum supported version; DCP requires an extension that supports protocol version %s or newer", version20240303)
	}

	req, reqCancel, reqErr := r.makeRequest(ideRunSessionResourcePath, http.MethodPut, bytes.NewBuffer(isrBody))
	if reqErr != nil {
		return nil, nil, fmt.Errorf("failed to create IDE run session request: %w", reqErr)
	}
	return req, reqCancel, nil
}

func (r *IdeExecutableRunner) prepareRunRequestV1(exe *apiv1.Executable, runInfo *controllers.ExecutableRunInfo) ([]byte, error) {
	if exe.Annotations[launchConfigurationsAnnotation] != "" {
		launchConfigsRaw := exe.Annotations[launchConfigurationsAnnotation]
		var launchConfigs json.RawMessage
		unmarshalErr := json.Unmarshal([]byte(launchConfigsRaw), &launchConfigs)
		if unmarshalErr != nil {
			return nil, fmt.Errorf("Executable cannot be run because its launch configuration is invalid: %w", unmarshalErr)
		}

		isr := ideRunSessionRequestV1{
			LaunchConfigurations: json.RawMessage(launchConfigs),
			Env:                  runInfo.EffectiveEnv,
			Args:                 runInfo.EffectiveArgs,
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

	runState := r.fetchRunStateForDeletion(runID)
	runState.runID = runID
	runState.pid = stn.PID
	runState.exitCode = exitCode
	runState.CloseOutputWriters()
	if !runState.finished {
		close(runState.exitCh)
	}
	runState.finished = true
	runState.NotifyRunCompletedAsync(r.lock)
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
