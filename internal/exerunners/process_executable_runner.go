/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package exerunners

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
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

type processRunState struct {
	identityTime time.Time
	stdOutFile   *os.File
	stdErrFile   *os.File
	cmdInfo      string // Command line used to start the process, for logging purposes
}

type ProcessExecutableRunner struct {
	pe               process.Executor
	runningProcesses *syncmap.Map[controllers.RunID, *processRunState]
}

func NewProcessExecutableRunner(pe process.Executor) *ProcessExecutableRunner {
	return &ProcessExecutableRunner{
		pe:               pe,
		runningProcesses: &syncmap.Map[controllers.RunID, *processRunState]{},
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

	stdOutFile, stdOutFileErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdOutFileErr != nil {
		startLog.Error(stdOutFileErr, "Failed to create temporary file for capturing process standard output data")
	} else {
		cmd.Stdout = usvc_io.NewTimestampWriter(stdOutFile)
		result.StdOutFile = stdOutFile.Name()
	}

	stdErrFile, stdErrFileErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdErrFileErr != nil {
		startLog.Error(stdErrFileErr, "Failed to create temporary file for capturing process standard error data")
	} else {
		cmd.Stderr = usvc_io.NewTimestampWriter(stdErrFile)
		result.StdErrFile = stdErrFile.Name()
	}

	var processExitHandler = process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
		ec := new(int32)
		*ec = exitCode

		runID := pidToRunID(pid)

		if runState, found := r.runningProcesses.LoadAndDelete(runID); found {
			for _, f := range []io.Closer{runState.stdOutFile, runState.stdErrFile} {
				if f != nil {
					if closeErr := f.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
						err = errors.Join(closeErr, err)
					}
				}
			}
		}

		if runChangeHandler != nil {
			runChangeHandler.OnRunCompleted(runID, ec, err)
		}
	})

	// We want to ensure that the service process tree is killed when DCP is stopped so that ports are released etc.
	pid, processIdentityTime, startWaitForProcessExit, startErr := r.pe.StartProcess(ctx, cmd, processExitHandler, process.CreationFlagEnsureKillOnDispose)
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
		// Use original log here, the watcher is a different process.
		dcpproc.RunProcessWatcher(r.pe, pid, processIdentityTime, log)

		r.runningProcesses.Store(pidToRunID(pid), &processRunState{
			identityTime: processIdentityTime,
			stdOutFile:   stdOutFile,
			stdErrFile:   stdErrFile,
			cmdInfo:      cmd.String(),
		})

		result.RunID = pidToRunID(pid)
		pointers.SetValue(&result.Pid, int64(pid))
		result.ExeState = apiv1.ExecutableStateRunning
		result.CompletionTimestamp = metav1.NewMicroTime(process.StartTimeForProcess(pid))
		result.StartWaitForRunCompletion = startWaitForProcessExit

		runChangeHandler.OnStartupCompleted(exe.NamespacedName(), result)

		return result
	}
}

func (r *ProcessExecutableRunner) StopRun(ctx context.Context, runID controllers.RunID, log logr.Logger) error {
	runState, found := r.runningProcesses.LoadAndDelete(runID)
	if !found {
		log.V(1).Info("Stop of a process run requested, but the run was already stopped", "RunID", runID)
		return nil
	}

	stopLog := log.WithValues("RunID", runID, "Command", runState.cmdInfo)
	stopLog.V(1).Info("Stopping run...")

	// We want to make progress eventually, so we don't want to wait indefinitely for the process to stop.
	const ProcessStopTimeout = 15 * time.Second

	timer := time.NewTimer(ProcessStopTimeout)
	defer timer.Stop()
	errCh := make(chan error, 1)

	go func() {
		if osutil.IsWindows() {
			// See StartRun() for why we need to use separate console for the app process on Windows.
			// This means we cannot send Ctrl-C to that process directly and need to use dcpproc StopProcessTree facility instead.
			stopCtx, stopCtxCancel := context.WithTimeout(ctx, ProcessStopTimeout)
			defer stopCtxCancel()
			errCh <- dcpproc.StopProcessTree(stopCtx, r.pe, runIdToPID(runID), runState.identityTime, stopLog)
		} else {
			errCh <- r.pe.StopProcess(runIdToPID(runID), runState.identityTime)
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
		for _, f := range []io.Closer{runState.stdOutFile, runState.stdErrFile} {
			if f != nil {
				if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					stopErr = errors.Join(stopErr, err)
				}
			}
		}
	}

	if stopErr != nil {
		stopLog.Error(stopErr, "Failed to stop run")
	}
	return stopErr
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

func runIdToPID(runID controllers.RunID) process.Pid_t {
	pid64, err := strconv.ParseInt(string(runID), 10, 64)
	if err != nil {
		return process.UnknownPID
	}
	pid, err := process.Int64_ToPidT(pid64)
	if err != nil {
		return process.UnknownPID
	}
	return pid
}

var _ controllers.ExecutableRunner = (*ProcessExecutableRunner)(nil)
