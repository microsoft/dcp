package exerunners

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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
	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/resiliency"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
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
	notifySocket    *websocket.Conn
	lock            *sync.Mutex
	startupQueue    *resiliency.WorkQueue // Queue for starting IDE run sessions
	activeRuns      syncmap.Map[controllers.RunID, *runState]
	log             logr.Logger
	lifetimeCtx     context.Context   // Lifetime context of the controller hosting this runner
	portStr         string            // The local port on which the IDE is listening for run session requests
	tokenStr        string            // The security token to use when connecting to the IDE
	client          http.Client       // The client to make HTTP requests to the IDE
	wsDialer        *websocket.Dialer // The websocket client to make requests to the IDE
	httpScheme      string            // The scheme to use when connecting to the IDE (HTTP or HTTPS)
	webSocketScheme string            // The scheme to use when connecting to the IDE via websockets (ws or wss)

	// The value of the api-version parameter, indicating protocol version the runner is using when talking tot the IDE
	// If empty, it indicates "pre-Aspire GA" (obsolete) protocol version.
	// TODO: remove pre-Aspire GA support after Aspire GA is released.
	apiVersion string
}

func NewIdeExecutableRunner(lifetimeCtx context.Context, log logr.Logger) (*IdeExecutableRunner, error) {
	const runnerNotAvailable = "Executables cannot be started via IDE: "
	const missingRequiredEnvVar = "missing required environment variable '%s'"

	createAndLogError := func(format string, a ...any) error {
		err := fmt.Errorf(format, a...)
		log.Info(runnerNotAvailable + err.Error())
		return err
	}

	portStr, found := os.LookupEnv(ideEndpointPortVar)
	if !found {
		return nil, createAndLogError(missingRequiredEnvVar, ideEndpointPortVar)
	}
	portStr = strings.TrimSpace(portStr)
	portStr = strings.TrimPrefix(portStr, "localhost:") // Visual Studio prepends "localhost:" to port number.

	tokenStr, found := os.LookupEnv(ideEndpointTokenVar)
	if !found {
		return nil, createAndLogError(missingRequiredEnvVar, ideEndpointTokenVar)
	}
	tokenStr = strings.TrimSpace(tokenStr)

	client := http.Client{}
	wsDialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: defaultIdeEndpointRequestTimeout,
	}
	httpScheme := "http"
	webSocketScheme := "ws"
	apiVersion := ""

	serverCertEncodedBytes, certFound := os.LookupEnv(ideEndpointCertVar)
	if certFound {
		certBytes, decodeErr := base64.StdEncoding.AppendDecode(nil, []byte(serverCertEncodedBytes))
		if decodeErr != nil {
			return nil, createAndLogError("failed to decode the server certificate: %w Secure communication with the IDE is not possible", decodeErr)
		}

		cert, certParseErr := x509.ParseCertificate(certBytes)
		if certParseErr != nil {
			return nil, createAndLogError("failed to decode the server certificate: %w Secure communication with the IDE is not possible", certParseErr)
		}

		caCertPool := x509.NewCertPool()
		caCertPool.AddCert(cert)
		tlsConfig := &tls.Config{
			RootCAs: caCertPool,
		}
		client.Transport = &http.Transport{
			TLSClientConfig: tlsConfig,
		}
		wsDialer.TLSClientConfig = tlsConfig
		httpScheme = "https"
		webSocketScheme = "wss"
	}

	// Query for supported protocol version
	req, reqCancel, reqCreationErr := makeIdeRequest(
		lifetimeCtx,
		httpScheme,
		portStr,
		ideRunSessionInfoPath,
		http.MethodGet,
		tokenStr,
		"",  // No API version--we are making this call to determine the API version to use,
		nil, // No body for this request
	)
	if reqCreationErr != nil {
		return nil, createAndLogError("failed to create IDE endpoint info request: %w", reqCreationErr)
	}
	defer reqCancel()

	clientResp, reqErr := client.Do(req)

	// We fall back to pre-Aspire GA protocol version if the request fails.
	if reqErr == nil && clientResp.StatusCode == http.StatusOK {
		defer clientResp.Body.Close()
		respBody, bodyReadErr := io.ReadAll(clientResp.Body)
		if bodyReadErr == nil {
			var info infoResponse
			unmarshalErr := json.Unmarshal(respBody, &info)
			if unmarshalErr != nil {
				log.Error(unmarshalErr, "failed to parse IDE info response")
			} else if slices.Contains(info.ProtocolsSupported, version20240303) {
				apiVersion = version20240303
			}
		}
	}

	return &IdeExecutableRunner{
		lock:            &sync.Mutex{},
		startupQueue:    resiliency.NewWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency),
		activeRuns:      syncmap.Map[controllers.RunID, *runState]{},
		log:             log,
		lifetimeCtx:     lifetimeCtx,
		portStr:         portStr,
		tokenStr:        tokenStr,
		client:          client,
		wsDialer:        wsDialer,
		httpScheme:      httpScheme,
		webSocketScheme: webSocketScheme,
		apiVersion:      apiVersion,
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
		resp, err = r.client.Do(req)
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

	resp, err := r.client.Do(req)
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

	if r.apiVersion == version20240303 {
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

func (r *IdeExecutableRunner) ensureNotificationSocket() error {
	r.lock.Lock()
	if r.notifySocket != nil {
		r.lock.Unlock()
		return nil
	}
	r.lock.Unlock()

	headers := http.Header{}
	headers.Add("Authorization", fmt.Sprintf("Bearer %s", r.tokenStr))
	var url string
	if r.apiVersion == version20240303 {
		url = fmt.Sprintf("%s://localhost:%s%s?%s=%s", r.webSocketScheme, r.portStr, ideRunSessionNotificationResourcePath, queryParamApiVersion, version20240303)
	} else {
		url = fmt.Sprintf("%s://localhost:%s%s", r.webSocketScheme, r.portStr, ideRunSessionNotificationResourcePath)
	}

	socket, _, err := r.wsDialer.Dial(url, headers)
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

func (r *IdeExecutableRunner) makeRequest(
	requestPath string,
	httpMethod string,
	body io.Reader,
) (*http.Request, context.CancelFunc, error) {
	return makeIdeRequest(
		r.lifetimeCtx,
		r.httpScheme,
		r.portStr,
		requestPath,
		httpMethod,
		r.tokenStr,
		r.apiVersion,
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

func makeIdeRequest(
	parentCtx context.Context,
	uriScheme string,
	portStr string,
	requestPath string,
	httpMethod string,
	securityToken string,
	apiVersion string,
	body io.Reader,
) (*http.Request, context.CancelFunc, error) {
	var url string
	if apiVersion != "" {
		url = fmt.Sprintf("%s://localhost:%s%s?%s=%s", uriScheme, portStr, requestPath, queryParamApiVersion, apiVersion)
	} else {
		url = fmt.Sprintf("%s://localhost:%s%s", uriScheme, portStr, requestPath)
	}
	reqCtx, reqCtxCancel := context.WithTimeout(parentCtx, defaultIdeEndpointRequestTimeout)

	req, reqCreationErr := http.NewRequestWithContext(reqCtx, httpMethod, url, body)
	if reqCreationErr != nil {
		reqCtxCancel()
		return nil, nil, fmt.Errorf("failed to create IDE endpoint info request: %w", reqCreationErr)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", securityToken))

	// Assume the body is JSON if it is provided
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, reqCtxCancel, nil
}

var _ controllers.ExecutableRunner = (*IdeExecutableRunner)(nil)
