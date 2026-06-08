/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/microsoft/dcp/api/v1"
	"github.com/microsoft/dcp/controllers"
	"github.com/microsoft/dcp/internal/dcpproc"
	"github.com/microsoft/dcp/internal/logs"
	"github.com/microsoft/dcp/internal/termpty"
	usvc_io "github.com/microsoft/dcp/pkg/io"
	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/pointers"
	"github.com/microsoft/dcp/pkg/process"
	"github.com/microsoft/dcp/pkg/slices"
	"github.com/microsoft/dcp/pkg/syncmap"
)

const (
	DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR = "DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR"

	persistentExecutableOutputDirName = "dcp-peo"
)

var persistentExecutableOutputDir = func() (string, error) {
	isAdmin, adminErr := osutil.IsAdmin()
	if adminErr != nil {
		return "", fmt.Errorf("could not determine current elevation: %w", adminErr)
	}
	baseDir := persistentExecutableOutputBaseDir()
	if isAdmin {
		return baseDir + "-admin", nil
	}
	return baseDir, nil
}

func persistentExecutableOutputBaseDir() string {
	if outputDir, found := os.LookupEnv(DCP_PERSISTENT_EXECUTABLE_OUTPUT_DIR); found && strings.TrimSpace(outputDir) != "" {
		return strings.TrimSpace(outputDir)
	}
	return filepath.Join(os.TempDir(), persistentExecutableOutputDirName)
}

type processRunState struct {
	pid          process.Pid_t
	identityTime time.Time

	// File handles for the process's output (applicable to non-terminal processes)
	stdOutFile *os.File
	stdErrFile *os.File

	// For terminal-using processes, the process and its connection manager for handling terminal connections.
	ptp     *termpty.PseudoTerminalProcess
	connMgr *termpty.ConnManager

	// Command line used to start the process, for logging purposes
	cmdInfo string

	// Adopted (persistent) process data
	adopted     bool
	cancelWatch func() // If the process is adopted, this function can be used to cancel the watch over it.

	// Change handler for notifying the Executable controller of process state changes.
	runChangeHandler controllers.RunChangeHandler
}

type ProcessExecutableRunner struct {
	pe                     process.Executor
	runningProcesses       *syncmap.ComparableValueMap[controllers.RunID, *processRunState]
	terminalProcessFactory termpty.TerminalProcessFactory

	// Do not use dcpproc.StopProcessTree, always go directly to process runner for stoppin the process.
	// Used for testing only.
	disableConsoleStop bool
}

func NewProcessExecutableRunner(pe process.Executor) *ProcessExecutableRunner {
	return &ProcessExecutableRunner{
		pe:                     pe,
		runningProcesses:       &syncmap.ComparableValueMap[controllers.RunID, *processRunState]{},
		terminalProcessFactory: termpty.StartProcessWithTerminal,
	}
}

// SetTerminalProcessFactory overrides the function used to spawn a process attached
// to a pseudo-terminal. Intended for tests that need to substitute a TestPty-backed
// implementation. Passing nil restores the default termpty.StartProcessWithTerminal.
func (r *ProcessExecutableRunner) SetTerminalProcessFactory(factory termpty.TerminalProcessFactory) {
	if factory == nil {
		factory = termpty.StartProcessWithTerminal
	}
	r.terminalProcessFactory = factory
}

func (r *ProcessExecutableRunner) StartRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) *controllers.ExecutableStartResult {
	cmd, creationFlags, processCtx := r.makeProcessCommand(ctx, exe)

	startLog := log.WithValues("Cmd", cmd.Path, "Args", cmd.Args[1:])
	startLog.Info("Starting process...")
	startLog.V(1).Info("Process details", "Env", cmd.Env, "Cwd", cmd.Dir)

	if exe.Spec.Terminal != nil {
		return r.startTerminalRun(ctx, processCtx, exe, cmd, creationFlags, runChangeHandler, startLog)
	} else {
		return r.startProcessRun(ctx, processCtx, exe, cmd, creationFlags, runChangeHandler, startLog)
	}
}

// makeProcessCommand performs the front-end work shared by the regular process start
// path and the terminal-attached process start path: it builds the *exec.Cmd, applies
// Windows console isolation, and chooses the appropriate creation flags and process
// context for the executable spec.
func (r *ProcessExecutableRunner) makeProcessCommand(
	ctx context.Context,
	exe *apiv1.Executable,
) (*exec.Cmd, process.ProcessCreationFlag, context.Context) {
	cmd := makeCommand(exe)
	if osutil.IsWindows() && exe.Spec.Terminal == nil {
		// On Windows we have seen some apps (e.g. Python uvicorn runner) sending Ctrl-C to the whole console group
		// to facilitate app reload after code change. This kills the DCP controller process unless we run the app
		// in an isolated manner, which includes a separate console.
		//
		// Terminal-attached executables will get their own console as part of terminal-support setup,
		// so they are not subject to the same console isolation.
		process.ForkFromParent(cmd)
	}

	var creationFlags process.ProcessCreationFlag = process.CreationFlagEnsureKillOnDispose
	processCtx := ctx
	if exe.Spec.Persistent {
		creationFlags = process.CreationFlagsNone
		processCtx = context.WithoutCancel(ctx)
	}

	return cmd, creationFlags, processCtx
}

// startProcessRun runs the regular (non-terminal) process start path: it captures
// stdout/stderr to files, starts the process via the executor, and registers the run
// in the runningProcesses map.
func (r *ProcessExecutableRunner) startProcessRun(
	ctx context.Context,
	processCtx context.Context,
	exe *apiv1.Executable,
	cmd *exec.Cmd,
	creationFlags process.ProcessCreationFlag,
	runChangeHandler controllers.RunChangeHandler,
	startLog logr.Logger,
) *controllers.ExecutableStartResult {
	result := controllers.NewExecutableStartResult()

	stdOutFile, stdOutFileErr := openExecutableOutputFile(exe, "out")
	if stdOutFileErr != nil {
		startLog.Error(stdOutFileErr, "Failed to create temporary file for capturing process standard output data")
	} else {
		cmd.Stdout = executableOutputWriter(exe, stdOutFile)
		result.StdOutFile = stdOutFile.Name()
	}

	stdErrFile, stdErrFileErr := openExecutableOutputFile(exe, "err")
	if stdErrFileErr != nil {
		startLog.Error(stdErrFileErr, "Failed to create temporary file for capturing process standard error data")
	} else {
		cmd.Stderr = executableOutputWriter(exe, stdErrFile)
		result.StdErrFile = stdErrFile.Name()
	}

	var processExitHandler = process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		ec := new(int32)
		*ec = exitCode

		runID := pidToRunID(pid)

		if runState, found := r.runningProcesses.LoadAndDelete(runID); found {
			err = errors.Join(closeProcessRunResources(runState), err)
		}

		if runChangeHandler != nil {
			runChangeHandler.OnRunCompleted(runID, ec, err)
		}
	})

	pid, processIdentityTime, startWaitForProcessExit, startErr := r.pe.StartProcess(processCtx, cmd, processExitHandler, creationFlags, nil)
	if startErr != nil {
		startLog.Error(startErr, "Failed to start a process")
		result.CompletionTimestamp = metav1.NowMicro()
		result.ExeState = apiv1.ExecutableStateFailedToStart
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

		result.StartupError = startErr
		runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)
		return result
	}

	if !exe.Spec.Persistent {
		// Use original log here, the watcher is a different process.
		dcpproc.RunProcessWatcher(r.pe, pid, processIdentityTime, startLog)
	} else if monitor, found, monitorErr := dcpproc.MonitorTargetFromFields(exe.Spec.MonitorPID, exe.Spec.MonitorTimestamp); monitorErr != nil {
		startLog.Error(monitorErr, "Could not start persistent Executable lifecycle monitor")
	} else if found {
		// Use original log here, the watcher is a different process.
		dcpproc.RunProcessWatcherForMonitor(r.pe, monitor, pid, processIdentityTime, startLog)
	}

	r.runningProcesses.Store(pidToRunID(pid), &processRunState{
		pid:              pid,
		identityTime:     processIdentityTime,
		stdOutFile:       stdOutFile,
		stdErrFile:       stdErrFile,
		cmdInfo:          cmd.String(),
		runChangeHandler: runChangeHandler,
	})

	displayStartTime := process.StartTimeForProcess(pid)
	result.RunID = pidToRunID(pid)
	pointers.SetValue(&result.Pid, int64(pid))
	result.ExeState = apiv1.ExecutableStateRunning
	result.CompletionTimestamp = metav1.NewMicroTime(displayStartTime)
	result.ProcessIdentityTime = processIdentityTime
	result.StartWaitForRunCompletion = startWaitForProcessExit

	runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)

	return result
}

// startTerminalRun runs the terminal-attached process start path: it spawns the
// process attached to a freshly allocated pseudo-terminal, then stands up a
// termpty.ConnManager bound to the configured UDS path so HMP v1 clients can
// attach to the terminal. The process and its PTY/ConnManager are owned by the
// runner for the lifetime of the run, and a dedicated goroutine waits for the
// process to exit so the run can be removed from runningProcesses and the
// run-change handler can be notified.
//
// Persistent + terminal is rejected at the API validation layer, so this path
// is reachable only for non-persistent Executables; we still honor the
// creationFlags / processCtx returned by makeProcessCommand for consistency.
func (r *ProcessExecutableRunner) startTerminalRun(
	ctx context.Context,
	processCtx context.Context,
	exe *apiv1.Executable,
	cmd *exec.Cmd,
	creationFlags process.ProcessCreationFlag,
	runChangeHandler controllers.RunChangeHandler,
	startLog logr.Logger,
) *controllers.ExecutableStartResult {
	result := controllers.NewExecutableStartResult()

	terminalSpec := exe.Spec.Terminal
	commandSpec := &termpty.CommandSpec{
		Cmd:           cmd,
		CreationFlags: creationFlags,
		Cols:          uint16(terminalSpec.Cols),
		Rows:          uint16(terminalSpec.Rows),
	}

	socketMode := termpty.SocketModeListen
	if terminalSpec.SocketMode.Normalized() == apiv1.TerminalSocketModeConnect {
		socketMode = termpty.SocketModeConnect
	}
	initialCols, initialRows := termpty.NormalizeTerminalDimensions(terminalSpec.Cols, terminalSpec.Rows)
	connMgr, connMgrErr := termpty.NewConnManager(
		processCtx,
		nil,
		terminalSpec.UDSPath,
		socketMode,
		initialCols,
		initialRows,
		startLog,
	)
	if connMgrErr != nil {
		startLog.Error(connMgrErr, "Failed to create terminal connection manager for the process")
		result.CompletionTimestamp = metav1.NowMicro()
		result.ExeState = apiv1.ExecutableStateFailedToStart
		result.StartupError = connMgrErr
		runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)
		return result
	}

	ptp, startErr := r.terminalProcessFactory(processCtx, r.pe, commandSpec)
	if startErr != nil {
		startLog.Error(startErr, "Failed to start a process attached to a pseudo-terminal")
		result.CompletionTimestamp = metav1.NowMicro()
		result.ExeState = apiv1.ExecutableStateFailedToStart
		result.StartupError = startErr
		runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)
		return result
	}

	attachErr := connMgr.AttachProcess(ptp)
	if attachErr != nil {
		startLog.Error(attachErr, "Failed to attach the process to the terminal connection manager; stopping process")
		// Best-effort: stop the just-started process and close its PTY before reporting failure.
		if stopErr := ptp.Stop(); stopErr != nil {
			startLog.Error(stopErr, "Failed to stop process after terminal connection manager creation failure")
		}
		if closeErr := ptp.PTY.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			startLog.Error(closeErr, "Failed to close PTY after terminal connection manager creation failure")
		}
		result.CompletionTimestamp = metav1.NowMicro()
		result.ExeState = apiv1.ExecutableStateFailedToStart
		result.StartupError = attachErr
		runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)
		return result
	}

	runID := pidToRunID(ptp.PID)
	r.runningProcesses.Store(runID, &processRunState{
		pid:              ptp.PID,
		identityTime:     ptp.IdentityTime,
		ptp:              ptp,
		connMgr:          connMgr,
		cmdInfo:          cmd.String(),
		runChangeHandler: runChangeHandler,
	})

	// Watch for process exit so we can deregister the run, tear down the PTY/ConnManager
	// resources, and notify the run-change handler.
	go r.watchTerminalRunExit(processCtx, runID, ptp, connMgr, runChangeHandler, startLog)

	displayStartTime := process.StartTimeForProcess(ptp.PID)
	result.RunID = runID
	pointers.SetValue(&result.Pid, int64(ptp.PID))
	result.ExeState = apiv1.ExecutableStateRunning
	result.CompletionTimestamp = metav1.NewMicroTime(displayStartTime)
	result.ProcessIdentityTime = ptp.IdentityTime
	result.StartWaitForRunCompletion = ptp.StartWaitForExit

	runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)

	// Reference the unused ctx parameter (kept for symmetry with startProcessRun and to
	// allow future extensions that may need the caller-supplied context).
	_ = ctx

	return result
}

// watchTerminalRunExit waits for the terminal-attached process to exit, then performs
// cleanup and notifies the run-change handler. It is launched as a single goroutine per
// terminal run by startTerminalRun.
//
// OnRunCompleted is called unconditionally — even if the run state was already torn
// down by a prior StopRun/ReleaseRun. This matches the behavior of the non-terminal
// process exit handler and is what the controller relies on to transition the
// Executable out of the Stopping state.
func (r *ProcessExecutableRunner) watchTerminalRunExit(
	processCtx context.Context,
	runID controllers.RunID,
	ptp *termpty.PseudoTerminalProcess,
	connMgr *termpty.ConnManager,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) {
	select {
	case <-ptp.ExitHandler.Exited():
	case <-processCtx.Done():
		// Lifetime context cancelled before the process exited. ConnManager will shut
		// itself down via the same context cancellation; we still want to wait for
		// the exit handler so the run-change handler gets accurate exit info.
		<-ptp.ExitHandler.Exited()
	}

	exitInfo := ptp.ExitHandler.ExitInfo()
	ec := new(int32)
	*ec = exitInfo.ExitCode

	var closeErr error
	if runState, found := r.runningProcesses.LoadAndDelete(runID); found {
		// We are the first to observe the exit (no concurrent StopRun/ReleaseRun);
		// own the resource cleanup. If StopRun/ReleaseRun already deleted the state,
		// they also took care of closing the PTY and waiting on ConnManager — skip both.
		closeErr = closeProcessRunResources(runState)
		if closeErr != nil {
			log.Error(closeErr, "Failed to close terminal run resources after process exit", "RunID", runID)
		}

		// Wait for the ConnManager to fully shut down so socket-file cleanup is observable
		// to callers (notably tests). ConnManager triggers its own shutdown when it observes
		// the process exit, so this should resolve promptly.
		select {
		case <-connMgr.Done():
		case <-processCtx.Done():
		}
	}

	combinedErr := errors.Join(exitInfo.Err, closeErr)
	if runChangeHandler != nil {
		runChangeHandler.OnRunCompleted(runID, ec, combinedErr)
	}
}

func (r *ProcessExecutableRunner) AdoptRun(
	_ context.Context,
	run controllers.ExecutableRunAdoptionInfo,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) error {
	if run.RunID == controllers.UnknownRunID {
		return fmt.Errorf("cannot adopt a process run without a run ID")
	}
	if run.Pid == process.UnknownPID {
		return fmt.Errorf("cannot adopt process run %s without a valid PID", run.RunID)
	}
	if run.ProcessIdentityTime.IsZero() {
		return fmt.Errorf("cannot adopt process run %s without process identity time", run.RunID)
	}

	if _, findErr := process.FindProcess(run.Pid, run.ProcessIdentityTime); findErr != nil {
		return fmt.Errorf("cannot adopt process run %s: %w", run.RunID, findErr)
	}
	stopWatching := make(chan struct{})
	var stopWatchingOnce sync.Once
	cancelWatch := func() {
		stopWatchingOnce.Do(func() {
			close(stopWatching)
		})
	}

	r.runningProcesses.Store(run.RunID, &processRunState{
		pid:              run.Pid,
		identityTime:     run.ProcessIdentityTime,
		cmdInfo:          run.CommandInfo,
		adopted:          true,
		cancelWatch:      cancelWatch,
		runChangeHandler: runChangeHandler,
	})

	go r.watchAdoptedProcess(run.RunID, run.Pid, run.ProcessIdentityTime, stopWatching, log)

	return nil
}

func (r *ProcessExecutableRunner) watchAdoptedProcess(
	runID controllers.RunID,
	pid process.Pid_t,
	identityTime time.Time,
	stopWatching <-chan struct{},
	log logr.Logger,
) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-stopWatching:
			return

		case <-timer.C:
			if _, findErr := process.FindProcess(pid, identityTime); findErr != nil {
				runState, found := r.runningProcesses.Load(runID)
				if !found {
					return
				}
				if runState.pid != pid || !runState.identityTime.Equal(identityTime) {
					return
				}
				if !r.runningProcesses.CompareAndDelete(runID, runState) {
					return
				}

				waitErr := closeProcessRunResources(runState)
				if runState.runChangeHandler != nil {
					runState.runChangeHandler.OnRunCompleted(runID, apiv1.UnknownExitCode, waitErr)
				}
				if waitErr != nil {
					log.Error(waitErr, "Error watching adopted process", "RunID", runID, "PID", runState.pid)
				}
				return
			}
			timer.Reset(2 * time.Second)
		}
	}
}

func (r *ProcessExecutableRunner) StopRun(ctx context.Context, runID controllers.RunID, log logr.Logger) error {
	runState, found := r.runningProcesses.LoadAndDelete(runID)
	if !found {
		log.V(1).Info("Stop of a process run requested, but the run was already stopped", "RunID", runID)
		return nil
	}
	if runState.cancelWatch != nil {
		runState.cancelWatch()
	}

	stopLog := log.WithValues("RunID", runID, "Command", runState.cmdInfo)
	stopLog.V(1).Info("Stopping run...")

	// We want to make progress eventually, so we don't want to wait indefinitely for the process to stop.
	const ProcessStopTimeout = 15 * time.Second

	timer := time.NewTimer(ProcessStopTimeout)
	defer timer.Stop()
	errCh := make(chan error, 1)

	go func() {
		if osutil.IsWindows() && !r.disableConsoleStop {
			// See StartRun() for why we need to use separate console for the app process on Windows.
			// This means we cannot send Ctrl-C to that process directly and need to use dcpproc StopProcessTree facility instead.
			stopCtx, stopCtxCancel := context.WithTimeout(ctx, ProcessStopTimeout)
			defer stopCtxCancel()
			errCh <- dcpproc.StopProcessTree(stopCtx, r.pe, runState.pid, runState.identityTime, stopLog)
		} else {
			errCh <- r.pe.StopProcess(runState.pid, runState.identityTime)
		}
	}()

	var stopErr error = nil
	select {
	case stopErr = <-errCh:
		// (no falltrough in Go)
	case <-timer.C:
		stopErr = fmt.Errorf("timed out waiting for process associated with run %s to stop", runID)
	}

	if found {
		stopErr = errors.Join(stopErr, closeProcessRunResources(runState))
		waitForConnManagerShutdown(ctx, runState, stopLog)
	}

	if stopErr != nil {
		stopLog.Error(stopErr, "Failed to stop run")
	} else if runState.adopted && runState.runChangeHandler != nil {
		runState.runChangeHandler.OnRunCompleted(runID, apiv1.UnknownExitCode, nil)
	}
	return stopErr
}

func (r *ProcessExecutableRunner) ReleaseRun(ctx context.Context, runID controllers.RunID, log logr.Logger) error {
	runState, found := r.runningProcesses.LoadAndDelete(runID)
	if !found {
		log.V(1).Info("Release of a process run requested, but the run was already released", "RunID", runID)
		return nil
	}
	if runState.cancelWatch != nil {
		runState.cancelWatch()
	}

	closeErr := closeProcessRunResources(runState)
	if closeErr != nil {
		log.Error(closeErr, "Could not close process run resources while releasing run", "RunID", runID)
	}
	waitForConnManagerShutdown(ctx, runState, log)
	return closeErr
}

// waitForConnManagerShutdown waits for a terminal run's ConnManager to finish
// shutting down (i.e. close its listener and any in-flight Serve loop, after
// which its background goroutines have terminated). It is a no-op for
// non-terminal runs. The wait is bounded by the supplied context so callers do
// not block indefinitely on a misbehaving manager.
//
// Note: ConnManager does not own the UDS socket file lifecycle, so the file
// at SocketPath() is not removed as part of shutdown.
func waitForConnManagerShutdown(ctx context.Context, runState *processRunState, log logr.Logger) {
	if runState.connMgr == nil {
		return
	}
	select {
	case <-runState.connMgr.Done():
	case <-ctx.Done():
		log.V(1).Info("Context cancelled while waiting for terminal connection manager to shut down")
	}
}

// closeProcessRunResources releases the OS resources owned by a process run:
// the stdout/stderr capture files (for non-terminal runs) and the PTY (for
// terminal runs). It is safe to call multiple times. The ConnManager owned by
// a terminal run is NOT closed here — its lifetime is governed by its parent
// context and by process-exit observation; callers that need to observe its
// shutdown should wait on connMgr.Done().
func closeProcessRunResources(runState *processRunState) error {
	var closeErr error
	for _, file := range []*os.File{runState.stdOutFile, runState.stdErrFile} {
		if file != nil {
			fileErr := file.Close()
			if fileErr != nil && !errors.Is(fileErr, os.ErrClosed) {
				closeErr = errors.Join(closeErr, fileErr)
			}
		}
	}
	if runState.ptp != nil && runState.ptp.PTY != nil {
		ptyErr := runState.ptp.PTY.Close()
		if ptyErr != nil && !errors.Is(ptyErr, os.ErrClosed) {
			closeErr = errors.Join(closeErr, ptyErr)
		}
	}
	return closeErr
}

func openExecutableOutputFile(exe *apiv1.Executable, stream string) (*os.File, error) {
	// Persistent output files are keyed by resource UID to keep paths unique across persistent Executable instances.
	// Kubernetes UIDs are effectively unique, so collisions between different Executable objects are not expected.
	fileName := fmt.Sprintf("%s_%s", exe.UID, stream)
	if !exe.Spec.Persistent {
		return usvc_io.OpenTempFile(fileName, os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	}

	dir, dirErr := persistentExecutableOutputDir()
	if dirErr != nil {
		return nil, fmt.Errorf("could not determine persistent Executable output directory: %w", dirErr)
	}
	if ensureDirErr := usvc_io.EnsureRestrictedDirectory(dir, osutil.PermissionOnlyOwnerReadWriteTraverse); ensureDirErr != nil {
		return nil, fmt.Errorf("could not prepare persistent Executable output directory: %w", ensureDirErr)
	}

	return usvc_io.OpenFile(filepath.Join(dir, fileName), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
}

func executableOutputWriter(exe *apiv1.Executable, file *os.File) usvc_io.WriteSyncerCloser {
	if exe.Spec.Persistent {
		return file
	}
	return usvc_io.NewTimestampWriter(file)
}

func makeCommand(exe *apiv1.Executable) *exec.Cmd {
	cmd := exec.Command(exe.Spec.ExecutablePath)
	cmd.Args = append([]string{exe.Spec.ExecutablePath}, exe.Status.EffectiveArgs...)

	cmd.Env = slices.Map[string](exe.Status.EffectiveEnv, func(e apiv1.EnvVar) string { return fmt.Sprintf("%s=%s", e.Name, e.Value) })

	cmd.Dir = exe.Spec.WorkingDirectory

	return cmd
}

func pidToRunID(pid process.Pid_t) controllers.RunID {
	return controllers.RunID(strconv.FormatInt(int64(pid), 10))
}

var _ controllers.ExecutableRunner = (*ProcessExecutableRunner)(nil)
var _ controllers.PersistentExecutableRunner = (*ProcessExecutableRunner)(nil)
