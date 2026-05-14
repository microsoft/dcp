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
	pid              process.Pid_t
	identityTime     time.Time
	stdOutFile       *os.File
	stdErrFile       *os.File
	cmdInfo          string // Command line used to start the process, for logging purposes
	adopted          bool
	cancelWatch      func()
	runChangeHandler controllers.RunChangeHandler
}

type ProcessExecutableRunner struct {
	pe               process.Executor
	runningProcesses *syncmap.ComparableValueMap[controllers.RunID, *processRunState]
}

func NewProcessExecutableRunner(pe process.Executor) *ProcessExecutableRunner {
	return &ProcessExecutableRunner{
		pe:               pe,
		runningProcesses: &syncmap.ComparableValueMap[controllers.RunID, *processRunState]{},
	}
}

func (r *ProcessExecutableRunner) StartRun(
	ctx context.Context,
	exe *apiv1.Executable,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) *controllers.ExecutableStartResult {
	cmd := makeCommand(exe)
	if osutil.IsWindows() {
		// On Windows we have seen some apps (e.g. Python uvicorn runner) sending Ctrl-C to the whole console group
		// to facilitate app reload after code change. This kills the DCP controller process unless we run the app
		// in an isolated manner, which includes a separate console.
		process.ForkFromParent(cmd)
	}

	startLog := log.WithValues("Cmd", cmd.Path, "Args", cmd.Args[1:])
	startLog.Info("Starting process...")
	startLog.V(1).Info("Process details", "Env", cmd.Env, "Cwd", cmd.Dir)
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
			err = errors.Join(closeProcessRunFiles(runState), err)
		}

		if runChangeHandler != nil {
			runChangeHandler.OnRunCompleted(runID, ec, err)
		}
	})

	var creationFlags process.ProcessCreationFlag = process.CreationFlagEnsureKillOnDispose
	processCtx := ctx
	if exe.Spec.Persistent {
		creationFlags = process.CreationFlagsNone
		processCtx = context.WithoutCancel(ctx)
	}

	pid, processIdentityTime, startWaitForProcessExit, startErr := r.pe.StartProcess(processCtx, cmd, processExitHandler, creationFlags)
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
	} else {
		if !exe.Spec.Persistent {
			// Use original log here, the watcher is a different process.
			dcpproc.RunProcessWatcher(r.pe, pid, processIdentityTime, log)
		} else if monitor, found, monitorErr := dcpproc.MonitorTargetFromFields(exe.Spec.MonitorPID, exe.Spec.MonitorTimestamp); monitorErr != nil {
			log.Error(monitorErr, "Could not start persistent Executable lifecycle monitor")
		} else if found {
			// Use original log here, the watcher is a different process.
			dcpproc.RunProcessWatcherForMonitor(r.pe, monitor, pid, processIdentityTime, log)
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

				waitErr := closeProcessRunFiles(runState)
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
		errCh <- process.StopViaConsole(stopLog, r.pe, runState.pid, runState.identityTime)
	}()

	var stopErr error = nil
	select {
	case stopErr = <-errCh:
		// (no falltrough in Go)
	case <-timer.C:
		stopErr = fmt.Errorf("timed out waiting for process associated with run %s to stop", runID)
	}

	if found {
		stopErr = errors.Join(stopErr, closeProcessRunFiles(runState))
	}

	if stopErr != nil {
		stopLog.Error(stopErr, "Failed to stop run")
	} else if runState.adopted && runState.runChangeHandler != nil {
		runState.runChangeHandler.OnRunCompleted(runID, apiv1.UnknownExitCode, nil)
	}
	return stopErr
}

func (r *ProcessExecutableRunner) ReleaseRun(_ context.Context, runID controllers.RunID, log logr.Logger) error {
	runState, found := r.runningProcesses.LoadAndDelete(runID)
	if !found {
		log.V(1).Info("Release of a process run requested, but the run was already released", "RunID", runID)
		return nil
	}
	if runState.cancelWatch != nil {
		runState.cancelWatch()
	}

	closeErr := closeProcessRunFiles(runState)
	if closeErr != nil {
		log.Error(closeErr, "Could not close process run files while releasing run", "RunID", runID)
	}
	return closeErr
}

func closeProcessRunFiles(runState *processRunState) error {
	var closeErr error
	for _, file := range []*os.File{runState.stdOutFile, runState.stdErrFile} {
		if file != nil {
			fileErr := file.Close()
			if fileErr != nil && !errors.Is(fileErr, os.ErrClosed) {
				closeErr = errors.Join(closeErr, fileErr)
			}
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
