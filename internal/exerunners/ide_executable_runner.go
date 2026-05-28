/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/ide"
	"github.com/microsoft/dcp/internal/logs"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/resiliency"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const (
	runSessionCouldNotBeStarted    = "run session could not be started: "
	launchConfigurationsAnnotation = "executable.usvc-dev.developer.microsoft.com/launch-configurations"
)

// IdeExecutableRunner runs Executables of ExecutionType=IDE by delegating to a
// shared ide.Client. It owns the Executable-specific concerns: looking up the
// launch-configurations annotation, creating per-run stdout/stderr temp files,
// bounding concurrent startup work, and bridging ide.SessionHandler callbacks
// to controllers.RunChangeHandler.
type IdeExecutableRunner struct {
	log          logr.Logger
	lifetimeCtx  context.Context
	ideClient    *ide.Client
	startupQueue *resiliency.WorkQueue // Bounds concurrent IDE run-session startups
	activeRuns   *syncmap.Map[controllers.RunID, *ideRunBridge]
}

// NewIdeExecutableRunner constructs an IdeExecutableRunner backed by the
// supplied shared ide.Client.
func NewIdeExecutableRunner(lifetimeCtx context.Context, ideClient *ide.Client, log logr.Logger) *IdeExecutableRunner {
	return &IdeExecutableRunner{
		log:          log,
		lifetimeCtx:  lifetimeCtx,
		ideClient:    ideClient,
		startupQueue: resiliency.NewWorkQueue(lifetimeCtx, resiliency.DefaultConcurrency),
		activeRuns:   &syncmap.Map[controllers.RunID, *ideRunBridge]{},
	}
}

func (r *IdeExecutableRunner) StartRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) *controllers.ExecutableStartResult {
	result := controllers.NewExecutableStartResult()

	if runChangeHandler == nil {
		result.ExeState = apiv1.ExecutableStateFailedToStart
		result.StartupError = fmt.Errorf(runSessionCouldNotBeStarted + "missing required runChangeHandler")
		return result
	}

	workEnqueueErr := r.startupQueue.Enqueue(func(_ context.Context) {
		r.doStartRun(ctx, exe, runChangeHandler, log)
	})

	if workEnqueueErr != nil {
		log.Error(workEnqueueErr, "Could not enqueue Executable start operation for IDE-based run")
		result.ExeState = apiv1.ExecutableStateFailedToStart
		result.StartupError = fmt.Errorf(runSessionCouldNotBeStarted+"%w", workEnqueueErr)
		return result
	}

	log.V(1).Info("Executable is starting...")
	result.ExeState = apiv1.ExecutableStateStarting
	return result
}

func (r *IdeExecutableRunner) doStartRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) {
	namespacedName := exe.NamespacedName()
	result := controllers.NewExecutableStartResult()
	var stdOutFile, stdErrFile *os.File

	reportRunCompletion := func(exeState apiv1.ExecutableState, startupError error) {
		result.CompletionTimestamp = metav1.NowMicro()
		result.ExeState = exeState
		result.StartupError = startupError

		if stdOutFile != nil {
			_ = stdOutFile.Close()
			_ = logs.RemoveWithRetry(ctx, result.StdOutFile)
			result.StdOutFile = ""
		}
		if stdErrFile != nil {
			_ = stdErrFile.Close()
			_ = logs.RemoveWithRetry(ctx, result.StdErrFile)
			result.StdErrFile = ""
		}

		runChangeHandler.OnStartupCompleted(namespacedName, result)
	}

	launchConfigs, lcErr := extractLaunchConfigurations(exe)
	if lcErr != nil {
		log.Error(lcErr, runSessionCouldNotBeStarted+"failed to extract launch configurations")
		reportRunCompletion(apiv1.ExecutableStateFailedToStart, lcErr)
		return
	}

	// Set up temp files for capturing stdout and stderr. These files (if successfully created)
	// will be cleaned up by the Executable controller when the Executable is deleted.

	var fileErr error
	stdOutFile, fileErr = usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if fileErr != nil {
		log.Error(fileErr, "Failed to create temporary file for capturing standard output data")
		stdOutFile = nil
	} else {
		result.StdOutFile = stdOutFile.Name()
	}

	stdErrFile, fileErr = usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if fileErr != nil {
		log.Error(fileErr, "Failed to create temporary file for capturing standard error data")
		stdErrFile = nil
	} else {
		result.StdErrFile = stdErrFile.Name()
	}

	bridge := newIdeRunBridge(r, namespacedName, runChangeHandler, stdOutFile, stdErrFile)

	startRes, startErr := r.ideClient.StartSession(ctx, ide.StartSessionRequest{
		LaunchConfigurations: launchConfigs,
		Env:                  exe.Status.EffectiveEnv,
		Args:                 exe.Status.EffectiveArgs,
	}, bridge, log)
	if startErr != nil {
		log.Error(startErr, runSessionCouldNotBeStarted)
		reportRunCompletion(apiv1.ExecutableStateFailedToStart, startErr)
		return
	}

	runID := controllers.RunID(startRes.SessionID)
	bridge.runID = runID

	if startRes.EarlyTermination != nil {
		// The IDE has reported the session as terminated before our StartSession returned.
		// The bridge was never invoked (and never will be), so we report completion ourselves.
		result.ExitCode = pointers.Duplicate(startRes.EarlyTermination.ExitCode)
		if startRes.EarlyTermination.Failed {
			result.ExeState = apiv1.ExecutableStateFailedToStart
		} else {
			result.ExeState = apiv1.ExecutableStateFinished
		}
		// In case of a failure we do not have an error to report here, but the IDE might have sent
		// some error messages via notifications, which would be already sent to the run change handler
		// (it wasn't, because the handler is the bridge that was never invoked — but logs are part of
		// the early-termination cleanup and there is no useful content to preserve).
		reportRunCompletion(result.ExeState, nil)
		return
	}

	r.activeRuns.Store(runID, bridge)

	log.V(1).Info("IDE run session started", "RunID", runID)
	result.RunID = runID
	result.CompletionTimestamp = metav1.NowMicro()
	result.ExeState = apiv1.ExecutableStateRunning
	result.StartWaitForRunCompletion = startRes.ConfirmHandlerReady

	// Hand off ownership of the files to the bridge; reportRunCompletion no longer cleans them up.
	stdOutFile = nil
	stdErrFile = nil

	runChangeHandler.OnStartupCompleted(namespacedName, result)
}

func (r *IdeExecutableRunner) StopRun(ctx context.Context, runID controllers.RunID, log logr.Logger) error {
	bridge, found := r.activeRuns.Load(runID)
	if !found {
		r.log.Info("Attempted to stop IDE run session which was not found", "RunID", runID)
		return nil
	}
	return r.ideClient.StopSession(ctx, bridge.sessionID(), log)
}

func (r *IdeExecutableRunner) ReleaseRun(ctx context.Context, runID controllers.RunID, log logr.Logger) error {
	bridge, found := r.activeRuns.LoadAndDelete(runID)
	if !found {
		log.V(1).Info("Release of an IDE run session requested, but the run was already released", "RunID", runID)
		return nil
	}
	bridge.closeOutputFiles()
	return r.ideClient.ReleaseSession(ctx, bridge.sessionID(), log)
}

// extractLaunchConfigurations returns the launch-configurations payload for an
// Executable (read from the well-known annotation) as raw JSON, ready to be
// passed to ide.Client.StartSession. The IDE-side validation (well-formed JSON,
// at least one supported type) is performed by ide.Client itself.
func extractLaunchConfigurations(exe *apiv1.Executable) (json.RawMessage, error) {
	raw := exe.Annotations[launchConfigurationsAnnotation]
	if raw == "" {
		return nil, fmt.Errorf("IDE execution was requested for the Executable but the required '%s' annotation is missing",
			launchConfigurationsAnnotation)
	}

	var lcs json.RawMessage
	if err := json.Unmarshal([]byte(raw), &lcs); err != nil {
		return nil, fmt.Errorf("Executable cannot be run because its launch configuration annotation is not valid JSON: %w", err)
	}
	return lcs, nil
}

// ideRunBridge bridges ide.SessionHandler callbacks for a single IDE run
// session to the controllers.RunChangeHandler that the Executable controller
// provided when StartRun was invoked. It also owns the stdout/stderr temp
// files associated with the run (wrapped in TimestampWriter so log lines are
// prefixed with line numbers and timestamps for parity with the process
// runner output).
type ideRunBridge struct {
	runner        *IdeExecutableRunner
	name          types.NamespacedName
	changeHandler controllers.RunChangeHandler

	// runID is set by IdeExecutableRunner.doStartRun once the session ID is
	// known and is read only on goroutines that observe it after StartSession
	// has returned successfully.
	runID controllers.RunID

	lock      *sync.Mutex
	stdOutW   usvc_io.WriteSyncerCloser
	stdErrW   usvc_io.WriteSyncerCloser
	outClosed bool
}

func newIdeRunBridge(
	runner *IdeExecutableRunner,
	name types.NamespacedName,
	changeHandler controllers.RunChangeHandler,
	stdOutFile, stdErrFile *os.File,
) *ideRunBridge {
	b := &ideRunBridge{
		runner:        runner,
		name:          name,
		changeHandler: changeHandler,
		lock:          &sync.Mutex{},
	}
	if stdOutFile != nil {
		b.stdOutW = usvc_io.NewTimestampWriter(stdOutFile)
	}
	if stdErrFile != nil {
		b.stdErrW = usvc_io.NewTimestampWriter(stdErrFile)
	}
	return b
}

func (b *ideRunBridge) sessionID() ide.SessionID {
	return ide.SessionID(b.runID)
}

func (b *ideRunBridge) OnProcessChanged(_ ide.SessionID, pid process.Pid_t) {
	b.changeHandler.OnMainProcessChanged(b.runID, pid)
}

func (b *ideRunBridge) OnTerminated(_ ide.SessionID, exitCode *int32) {
	b.closeOutputFiles()
	b.changeHandler.OnRunCompleted(b.runID, exitCode, nil)
}

func (b *ideRunBridge) OnLog(_ ide.SessionID, isStdErr bool, message string) {
	// ide.Client serializes per-session notifications, so this method runs
	// without races against other On* callbacks for the same session.
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.outClosed {
		return
	}

	target := b.stdOutW
	if isStdErr {
		target = b.stdErrW
	}
	if target == nil {
		return
	}

	_, err := target.Write(osutil.WithNewline([]byte(message)))
	if err != nil {
		b.runner.log.Error(err, "Failed to persist an IDE-emitted log message", "RunID", b.runID)
	}
}

func (b *ideRunBridge) OnMessage(_ ide.SessionID, level ide.MessageLevel, message string) {
	var rml controllers.RunMessageLevel
	switch level {
	case ide.MessageLevelInfo:
		rml = controllers.RunMessageLevelInfo
	case ide.MessageLevelDebug:
		rml = controllers.RunMessageLevelDebug
	case ide.MessageLevelError:
		rml = controllers.RunMessageLevelError
	default:
		rml = controllers.RunMessageLevelInfo
	}
	b.changeHandler.OnRunMessage(b.runID, rml, message)
}

func (b *ideRunBridge) closeOutputFiles() {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.outClosed {
		return
	}
	b.outClosed = true

	if b.stdOutW != nil {
		_ = b.stdOutW.Close()
		b.stdOutW = nil
	}
	if b.stdErrW != nil {
		_ = b.stdErrW.Close()
		b.stdErrW = nil
	}
}

var (
	_ controllers.ExecutableRunner = (*IdeExecutableRunner)(nil)
	_ ide.SessionHandler           = (*ideRunBridge)(nil)
)
