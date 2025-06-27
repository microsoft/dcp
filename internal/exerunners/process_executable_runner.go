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

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/internal/dcpproc"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/pointers"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type processRunState struct {
	startTime  time.Time
	stdOutFile *os.File
	stdErrFile *os.File
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
	runInfo *controllers.ExecutableRunInfo,
	runChangeHandler controllers.RunChangeHandler,
	log logr.Logger,
) error {
	cmd := makeCommand(exe)
	log.Info("starting process...", "executable", cmd.Path)
	log.V(1).Info("process settings",
		"executable", cmd.Path,
		"args", cmd.Args[1:],
		"env", cmd.Env,
		"cwd", cmd.Dir)

	stdOutFile, stdOutFileErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_out_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdOutFileErr != nil {
		log.Error(stdOutFileErr, "failed to create temporary file for capturing process standard output data")
	} else {
		cmd.Stdout = usvc_io.NewTimestampWriter(stdOutFile)
		runInfo.StdOutFile = stdOutFile.Name()
	}

	stdErrFile, stdErrFileErr := usvc_io.OpenTempFile(fmt.Sprintf("%s_err_%s", exe.Name, exe.UID), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if stdErrFileErr != nil {
		log.Error(stdErrFileErr, "failed to create temporary file for capturing process standard error data")
	} else {
		cmd.Stderr = usvc_io.NewTimestampWriter(stdErrFile)
		runInfo.StdErrFile = stdErrFile.Name()
	}

	var processExitHandler process.ProcessExitHandler = nil
	if runChangeHandler != nil {
		processExitHandler = process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
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

			runChangeHandler.OnRunCompleted(runID, ec, err)
		})
	}

	// We want to ensure that the service process tree is killed when DCP is stopped so that ports are released etc.
	pid, startTime, startWaitForProcessExit, startErr := r.pe.StartProcess(ctx, cmd, processExitHandler, process.CreationFlagEnsureKillOnDispose)
	if startErr != nil {
		log.Error(startErr, "failed to start a process")
		runInfo.FinishTimestamp = metav1.NowMicro()
		runInfo.ExeState = apiv1.ExecutableStateFailedToStart
		return startErr
	}

	log.Info("process started", "executable", cmd.Path, "PID", pid)

	dcpproc.RunWatcher(r.pe, pid, startTime, log)

	r.runningProcesses.Store(pidToRunID(pid), &processRunState{
		startTime:  startTime,
		stdOutFile: stdOutFile,
		stdErrFile: stdErrFile,
	})

	runInfo.RunID = pidToRunID(pid)
	pointers.SetValue(&runInfo.Pid, (*int64)(&pid))
	runInfo.ExeState = apiv1.ExecutableStateRunning
	runInfo.StartupTimestamp = metav1.NewMicroTime(startTime)

	runChangeHandler.OnStartupCompleted(exe.NamespacedName(), runInfo, startWaitForProcessExit)

	return nil
}

func (r *ProcessExecutableRunner) StopRun(_ context.Context, runID controllers.RunID, log logr.Logger) error {
	runState, found := r.runningProcesses.LoadAndDelete(runID)
	if !found {
		log.V(1).Info("run stop requested, but the run was already stopped", "runID", runID)
		return nil
	}

	log.V(1).Info("stopping process...", "runID", runID)

	// We want to make progress eventually, so we don't want to wait indefinitely for the process to stop.
	const ProcessStopTimeout = 15 * time.Second

	timer := time.NewTimer(ProcessStopTimeout)
	defer timer.Stop()
	errCh := make(chan error, 1)

	go func() {
		errCh <- r.pe.StopProcess(runIdToPID(runID), runState.startTime)
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

	return stopErr
}

func makeCommand(exe *apiv1.Executable) *exec.Cmd {
	cmd := exec.Command(exe.Spec.ExecutablePath)
	cmd.Args = append([]string{exe.Spec.ExecutablePath}, exe.Status.EffectiveArgs...)

	cmd.Env = slices.Map[apiv1.EnvVar, string](exe.Status.EffectiveEnv, func(e apiv1.EnvVar) string { return fmt.Sprintf("%s=%s", e.Name, e.Value) })

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
	pid, err := process.Int64ToPidT(pid64)
	if err != nil {
		return process.UnknownPID
	}
	return pid
}

var _ controllers.ExecutableRunner = (*ProcessExecutableRunner)(nil)
