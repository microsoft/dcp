package exerunners

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/go-logr/logr"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	usvc_io "github.com/microsoft/usvc-apiserver/pkg/io"
	"github.com/microsoft/usvc-apiserver/pkg/logger"
	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	"github.com/microsoft/usvc-apiserver/pkg/syncmap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type processRunState struct {
	stdOutFile *os.File
	stdErrFile *os.File
}

func newProcessRunState(stdOutFile *os.File, stdErrFile *os.File) *processRunState {
	return &processRunState{
		stdOutFile: stdOutFile,
		stdErrFile: stdErrFile,
	}
}

type ProcessExecutableRunner struct {
	pe               process.Executor
	runningProcesses syncmap.Map[controllers.RunID, *processRunState]
}

func NewProcessExecutableRunner(pe process.Executor) *ProcessExecutableRunner {
	return &ProcessExecutableRunner{
		pe:               pe,
		runningProcesses: syncmap.Map[controllers.RunID, *processRunState]{},
	}
}

func (r *ProcessExecutableRunner) StartRun(ctx context.Context, exe *apiv1.Executable, runChangeHandler controllers.RunChangeHandler, log logr.Logger) error {
	cmd := makeCommand(ctx, exe, log)
	log.Info("starting process...", "executable", cmd.Path)
	log.V(1).Info("process settings",
		"executable", cmd.Path,
		"args", cmd.Args[1:],
		"env", cmd.Env,
		"cwd", cmd.Dir)

	outputRootFolder := os.TempDir()
	if dcpSessionDir, found := os.LookupEnv(logger.DCP_SESSION_FOLDER); found {
		outputRootFolder = dcpSessionDir
	}

	stdOutFile, err := usvc_io.OpenFile(filepath.Join(outputRootFolder, fmt.Sprintf("%s_out_%s", exe.Name, exe.UID)), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing process standard output data")
	} else {
		cmd.Stdout = stdOutFile
		exe.Status.StdOutFile = stdOutFile.Name()
	}

	stdErrFile, err := usvc_io.OpenFile(filepath.Join(outputRootFolder, fmt.Sprintf("%s_err_%s", exe.Name, exe.UID)), os.O_RDWR|os.O_CREATE|os.O_EXCL, osutil.PermissionOnlyOwnerReadWrite)
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing process standard error data")
	} else {
		cmd.Stderr = stdErrFile
		exe.Status.StdErrFile = stdErrFile.Name()
	}

	var processExitHandler process.ProcessExitHandler = nil
	if runChangeHandler != nil {
		processExitHandler = process.ProcessExitHandlerFunc(func(pid process.Pid_t, exitCode int32, err error) {
			ec := new(int32)
			*ec = exitCode
			runChangeHandler.OnRunChanged(pidToRunID(pid), pid, ec, err)
		})
	}

	pid, startWaitForProcessExit, err := r.pe.StartProcess(ctx, cmd, processExitHandler)

	if err != nil {
		log.Error(err, "failed to start a process")
		exe.Status.FinishTimestamp = metav1.Now()
		exe.Status.State = apiv1.ExecutableStateFailedToStart
	} else {
		r.runningProcesses.Store(pidToRunID(pid), newProcessRunState(stdOutFile, stdErrFile))
		log.Info("process started", "executable", cmd.Path, "PID", pid)
		exe.Status.ExecutionID = pidToExecutionID(pid)
		if exe.Status.PID == apiv1.UnknownPID {
			exe.Status.PID = new(int64)
		}
		*exe.Status.PID = int64(pid)
		exe.Status.State = apiv1.ExecutableStateRunning
		exe.Status.StartupTimestamp = metav1.Now()

		runChangeHandler.OnStartingCompleted(exe.NamespacedName(), pidToRunID(pid), exe.Status, startWaitForProcessExit)
	}

	return err
}

func (r *ProcessExecutableRunner) StopRun(_ context.Context, runID controllers.RunID, log logr.Logger) error {
	log.V(1).Info("stopping process...", "runID", runID)
	err := r.pe.StopProcess(runIdToPID(runID))

	if runState, found := r.runningProcesses.LoadAndDelete(runID); found {
		var stdOutErr error
		if runState.stdOutFile != nil {
			stdOutErr = runState.stdOutFile.Close()
		}

		var stdErrErr error
		if runState.stdErrFile != nil {
			stdErrErr = runState.stdErrFile.Close()
		}

		err = errors.Join(err, stdOutErr, stdErrErr)
	}

	return err
}

func makeCommand(ctx context.Context, exe *apiv1.Executable, log logr.Logger) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe.Spec.ExecutablePath)
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

func pidToExecutionID(pid process.Pid_t) string {
	return strconv.FormatInt(int64(pid), 10)
}

var _ controllers.ExecutableRunner = (*ProcessExecutableRunner)(nil)
