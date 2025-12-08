// Copyright (c) Microsoft Corporation. All rights reserved.

package exerunners

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	"github.com/microsoft/usvc-apiserver/internal/logs"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/resiliency"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
)

const (
	runSessionCouldNotBeStarted = "run session could not be started: "
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
	activeRuns          *syncmap.Map[controllers.RunID, *runData]
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
		activeRuns:     &syncmap.Map[controllers.RunID, *runData]{},
		log:            log,
		lifetimeCtx:    lifetimeCtx,
		connectionInfo: connInfo,
	}

	nh := NewIdeNotificationHandler(lifetimeCtx, r, connInfo, log)
	r.notificationHandler = nh
	return r, nil
}

func (r *IdeExecutableRunner) StartRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *controllers.ExecutableRunInfo,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) error {
	if runChangeHandler == nil {
		return fmt.Errorf(runSessionCouldNotBeStarted + "missing required runChangeHandler")
	}

	ideConnErr := r.notificationHandler.WaitConnected(ctx)
	if ideConnErr != nil {
		return fmt.Errorf(runSessionCouldNotBeStarted+"%w", ideConnErr)
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	workEnqueueErr := r.startupQueue.Enqueue(func(_ context.Context) {
		r.doStartRun(ctx, exe, runInfo.Clone(), runChangeHandler, log)
	})

	if workEnqueueErr != nil {
		log.Error(workEnqueueErr, "Could not enqueue ide executable start operation; workload is shutting down")
		runInfo.ExeState = apiv1.ExecutableStateFailedToStart
		runInfo.FinishTimestamp = metav1.NowMicro()
	} else {
		log.V(1).Info("Executable is starting...")
		runInfo.ExeState = apiv1.ExecutableStateStarting
	}

	return nil
}

func (r *IdeExecutableRunner) doStartRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runInfo *controllers.ExecutableRunInfo,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) {
	var stdOutFile, stdErrFile *os.File
	namespacedName := exe.NamespacedName()
	runInfo.RunID = controllers.UnknownRunID

	reportRunCompletion := func(exeState apiv1.ExecutableState) {
		r.lock.Lock()
		defer r.lock.Unlock()

		now := metav1.NowMicro()
		runInfo.StartupTimestamp = now
		runInfo.FinishTimestamp = now
		runInfo.ExeState = exeState

		if stdOutFile != nil {
			_ = stdOutFile.Close()
			_ = logs.RemoveWithRetry(ctx, runInfo.StdOutFile)
			runInfo.StdOutFile = ""
		}
		if stdErrFile != nil {
			_ = stdErrFile.Close()
			_ = logs.RemoveWithRetry(ctx, runInfo.StdErrFile)
			runInfo.StdErrFile = ""
		}

		runChangeHandler.OnStartupCompleted(namespacedName, runInfo, func() {})
	}

	req, reqCancel, err := r.prepareRunRequest(exe)
	if err != nil {
		log.Error(err, runSessionCouldNotBeStarted+"failed to prepare run session request")
		reportRunCompletion(apiv1.ExecutableStateFailedToStart)
		return
	}
	defer reqCancel()

	// Set up temp files for capturing stdout and stderr. These files (if successfully created)
	// will be cleaned up by the Executable controller when the Executable is deleted.

	stdOutFile, err = usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "Failed to create temporary file for capturing standard output data")
		stdOutFile = nil
	} else {
		runInfo.StdOutFile = stdOutFile.Name()
	}

	stdErrFile, err = usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "Failed to create temporary file for capturing standard error data")
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
			log.Error(err, fmt.Sprintf("Timeout of %.0f seconds exceeded waiting for the IDE to start a run session; you can set the DCP_IDE_REQUEST_TIMEOUT_SECONDS environment variable to override this timeout (in seconds)", ideEndpointRequestTimeout.Seconds()))
		} else {
			log.Error(err, runSessionCouldNotBeStarted+"request round-trip failed")
		}

		reportRunCompletion(apiv1.ExecutableStateFailedToStart)
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
		reportRunCompletion(apiv1.ExecutableStateFailedToStart) // The IDE refused to run this Executable
		return
	}

	sessionURL := resp.Header.Get("Location")
	if sessionURL == "" {
		reportRunCompletion(apiv1.ExecutableStateFailedToStart)
		return
	}

	var rid string
	rid, err = getLastUrlPathSegment(sessionURL)
	if err != nil {
		reportRunCompletion(apiv1.ExecutableStateFailedToStart)
		return
	}

	r.lock.Lock()

	runID := controllers.RunID(rid)
	rd := r.ensureRunData(runID)
	rd.runID = runID
	if rd.state == runStateFailedToStart || rd.state == runStateCompleted {
		// We are not going to use the change handler for this run, but we might need to process some notifications about it,
		// so we do not want to block on the change handler wait group.
		rd.DisableChangeHandlerReadiness()

		r.activeRuns.Delete(runID)
		r.lock.Unlock()

		if rd.state == runStateFailedToStart {
			runInfo.ExeState = apiv1.ExecutableStateFailedToStart
		} else {
			runInfo.ExeState = apiv1.ExecutableStateFinished
		}
		reportRunCompletion(runInfo.ExeState)

		return
	}

	rd.name = namespacedName
	rd.sessionURL = sessionURL
	defer rd.IncreaseChangeHandlerReadiness()
	err = rd.SetOutputWriters(stdOutFile, stdErrFile)
	if err != nil {
		log.Error(err, "Failed to set output writers to capture stdout/stderr") // Should never happen
	}
	rd.changeHandler = runChangeHandler
	startWaitForRunCompletion := func() {
		rd.IncreaseChangeHandlerReadiness()
	}
	rd.startRunMethodCompleted = true

	r.lock.Unlock()

	log.V(1).Info("IDE run session started", "RunID", runID)
	runInfo.RunID = runID
	runInfo.StartupTimestamp = metav1.NowMicro()
	runInfo.ExeState = apiv1.ExecutableStateRunning
	runChangeHandler.OnStartupCompleted(namespacedName, runInfo, startWaitForRunCompletion)
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

func (r *IdeExecutableRunner) prepareRunRequest(exe *apiv1.Executable) (*http.Request, context.CancelFunc, error) {
	var isrBody []byte
	var bodyErr error

	if equalOrNewer(r.connectionInfo.apiVersion, version20240303) {
		isrBody, bodyErr = r.prepareRunRequestV1(exe)
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

func (r *IdeExecutableRunner) prepareRunRequestV1(exe *apiv1.Executable) ([]byte, error) {
	if exe.Annotations[launchConfigurationsAnnotation] != "" {
		launchConfigsRaw := exe.Annotations[launchConfigurationsAnnotation]
		var launchConfigs json.RawMessage
		unmarshalErr := json.Unmarshal([]byte(launchConfigsRaw), &launchConfigs)
		if unmarshalErr != nil {
			return nil, fmt.Errorf("Executable cannot be run because its launch configuration is invalid: %w", unmarshalErr)
		}

		// Check if at least one launch configuration is of a supported type
		var lcs []launchConfigurationBase
		unmarshalErr = json.Unmarshal(launchConfigs, &lcs)
		if unmarshalErr != nil {
			return nil, fmt.Errorf("Executable cannot be run because its launch configuration is invalid: %w", unmarshalErr)
		}
		requestedConfigurations := slices.Map[launchConfigurationBase, string](lcs, func(lc launchConfigurationBase) string { return lc.Type })
		if len(slices.Intersect(r.connectionInfo.supportedLaunchConfigurations, requestedConfigurations)) == 0 {
			return nil, fmt.Errorf("Executable cannot be run because its launch configuration type is not supported by the connected IDE; supported types: %v, requested types: %v",
				r.connectionInfo.supportedLaunchConfigurations, requestedConfigurations)
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

	rd := r.ensureRunData(runID)
	rd.runID = runID
	rd.pid = pcn.PID
	if rd.state == runStateNotStarted {
		rd.state = runStateRunning
	}

	rd.NotifyRunChangedAsync(r.lock)
}

func (r *IdeExecutableRunner) HandleSessionTermination(stn ideRunSessionTerminatedNotification) {
	runID := controllers.RunID(stn.SessionID)
	exitCode := apiv1.UnknownExitCode
	if stn.ExitCode != nil {
		if math.MinInt32 <= *stn.ExitCode && *stn.ExitCode <= math.MaxUint32 {
			// If the exit code can be represented as int32 without data loss, use it.
			// A reinterpretation of uint32 value will occur if the code > math.MaxInt32, but that is acceptable.
			exitCode = new(int32)
			*exitCode = int32(*stn.ExitCode)
		} else {
			r.log.Info("Received IDE run session termination notification with exit code outside uint32 range; will treat exit code as 'unknown'.",
				"RunID", runID,
				"ReceivedExitCode", *stn.ExitCode,
			)
		}
	}
	r.log.V(1).Info("IDE run session terminated", "RunID", runID, "PID", stn.PID, "ExitCode", exitCode)

	r.lock.Lock()
	defer r.lock.Unlock()

	rd := r.ensureRunData(runID)
	if rd.startRunMethodCompleted {
		// This is the last time we will hear about this run session, so we can delete it from the active runs map.
		// If startRunCompleted is false, this notification indicates "failed at startup" run and we need to keep its data
		// in the active runs till the StartRun() method has a chance to process (and delete) it.
		r.activeRuns.Delete(runID)
	}
	rd.runID = runID
	rd.pid = stn.PID
	rd.exitCode = exitCode
	rd.CloseOutputWriters()
	switch rd.state {
	case runStateNotStarted:
		rd.state = runStateFailedToStart
		close(rd.exitCh)
	case runStateRunning:
		rd.state = runStateCompleted
		close(rd.exitCh)
	}
	rd.NotifyRunCompletedAsync(r.lock)
}

func (r *IdeExecutableRunner) HandleServiceLogs(nsl ideSessionLogNotification) {
	runID := controllers.RunID(nsl.SessionID)

	rd := r.ensureRunData(runID)
	var err error
	msg := osutil.WithNewline([]byte(nsl.LogMessage))
	if nsl.IsStdErr {
		_, err = rd.stdErr.Write(msg)
	} else {
		_, err = rd.stdOut.Write(msg)
	}

	if err != nil {
		r.log.Error(err, "Failed to persist a log message")
	}
}

func (r *IdeExecutableRunner) HandleSessionMessage(smn ideSessionMessageNotification) {
	runID := controllers.RunID(smn.SessionID)
	log := r.log.WithValues("RunID", runID)

	msg := strings.TrimSpace(smn.Message)
	if msg == "" {
		log.V(1).Info("Received empty IDE session message, ignoring")
		return
	}

	rd := r.ensureRunData(runID)

	switch smn.Level {

	case sessionMessageLevelInfo:
		rd.SendRunMessageAsync(r.lock, msg, controllers.RunMessageLevelInfo)

	case sessionMessageLevelDebug:
		rd.SendRunMessageAsync(r.lock, msg, controllers.RunMessageLevelDebug)

	case sessionMessageLevelError:
		er := errorResponse{
			Error: errorDetail{
				Code:    smn.Code,
				Message: smn.Message,
				Details: smn.Details,
			},
		}
		rd.SendRunMessageAsync(r.lock, er.String(), controllers.RunMessageLevelError)

	default:
		log.V(1).Info("Received IDE session message with unexpected severity level", "Level", smn.Level, "Message", smn.Message)
	}
}

func (r *IdeExecutableRunner) ensureRunData(runID controllers.RunID) *runData {
	// Notifications for a particular run may arrive before the call to create a run session returns.
	// That is why we use LoadOrStoreNew in places where we want to ensure that we have a runState for a given run ID.
	rd, _ := r.activeRuns.LoadOrStoreNew(runID, func() *runData {
		return NewRunData(r.lifetimeCtx)
	})
	return rd
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
