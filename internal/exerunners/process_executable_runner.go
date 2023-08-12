package exerunners

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/go-logr/logr"
	"github.com/joho/godotenv"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/microsoft/usvc-apiserver/controllers"
	"github.com/microsoft/usvc-apiserver/pkg/maps"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ProcessExecutableRunner struct {
	pe process.Executor
}

func NewProcessExecutableRunner(pe process.Executor) *ProcessExecutableRunner {
	return &ProcessExecutableRunner{pe: pe}
}

func (r *ProcessExecutableRunner) StartRun(ctx context.Context, exe *apiv1.Executable, runCompletionHandler controllers.RunCompletionHandler, log logr.Logger) (controllers.RunID, func(), error) {
	cmd := makeCommand(ctx, exe, log)
	log.Info("starting process...", "executable", cmd.Path)
	log.V(1).Info("process settings",
		"executable", cmd.Path,
		"args", cmd.Args[1:],
		"env", cmd.Env,
		"cwd", cmd.Dir)

	stdOutFile, err := os.CreateTemp("", fmt.Sprintf("%s_out_*", exe.Name))
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing process standard output data")
	} else {
		cmd.Stdout = stdOutFile
		exe.Status.StdOutFile = stdOutFile.Name()
	}

	stdErrFile, err := os.CreateTemp("", fmt.Sprintf("%s_err_*", exe.Name))
	if err != nil {
		log.Error(err, "failed to create temporary file for capturing process standard error data")
	} else {
		cmd.Stderr = stdErrFile
		exe.Status.StdErrFile = stdErrFile.Name()
	}

	var processExitHandler process.ProcessExitHandler = nil
	if runCompletionHandler != nil {
		processExitHandler = process.ProcessExitHandlerFunc(func(pid int32, exitCode int32, err error) {
			runCompletionHandler.OnRunCompleted(pidToRunID(pid), exitCode, err)
		})
	}

	pid, startWaitForProcessExit, err := r.pe.StartProcess(ctx, cmd, processExitHandler)
	if err != nil {
		log.Error(err, "failed to start a process")
		exe.Status.FinishTimestamp = metav1.Now()
		exe.Status.State = apiv1.ExecutableStateFailedToStart
	} else {
		log.Info("process started", "executable", cmd.Path, "PID", pid)
		exe.Status.ExecutionID = pidToExecutionID(pid)
		exe.Status.State = apiv1.ExecutableStateRunning
		exe.Status.StartupTimestamp = metav1.Now()
	}

	return pidToRunID(pid), startWaitForProcessExit, err
}

func (r *ProcessExecutableRunner) StopRun(_ context.Context, runID controllers.RunID, _ logr.Logger) error {
	return r.pe.StopProcess(runIdToPID(runID))
}

func makeCommand(ctx context.Context, exe *apiv1.Executable, log logr.Logger) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe.Spec.ExecutablePath)
	cmd.Args = append([]string{exe.Spec.ExecutablePath}, exe.Spec.Args...)

	env := slices.Map[apiv1.EnvVar, string](exe.Spec.Env, func(e apiv1.EnvVar) string { return fmt.Sprintf("%s=%s", e.Name, e.Value) })

	additionalEnv, err := godotenv.Read(exe.Spec.EnvFiles...)
	if err != nil {
		log.Error(err, "Environment settings from .env file(s) were not applied.", "EnvFiles", exe.Spec.EnvFiles)
	} else {
		env = append(env, maps.MapToSlice[string, string, string](additionalEnv, func(key, val string) string { return fmt.Sprintf("%s=%s", key, val) })...)
	}

	cmd.Env = append(os.Environ(), env...) // Include parent process environment
	cmd.Dir = exe.Spec.WorkingDirectory
	return cmd

}

func pidToRunID(pid int32) controllers.RunID {
	return controllers.RunID(strconv.Itoa(int(pid)))
}

func runIdToPID(runID controllers.RunID) int32 {
	pid, err := strconv.ParseInt(string(runID), 10, 32)
	if err != nil {
		return apiv1.UnknownPID
	}
	return int32(pid)
}

func pidToExecutionID(pid int32) string {
	return strconv.Itoa(int(pid))
}

var _ controllers.ExecutableRunner = (*ProcessExecutableRunner)(nil)
