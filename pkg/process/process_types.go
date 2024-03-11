package process

import (
	"context"
	"os/exec"
)

const (
	// A valid exit code of a process is a non-negative number. We use UnknownExitCode to indicate that we have not obtained the exit code yet.
	UnknownExitCode int32 = -1

	// Unknown PID code is used when replica is not started (or fails to start)
	UnknownPID Pid_t = -1
)

// Pid_t is a type used to represent process IDs.
// The domain is all unsigned 32-bit integers, but we use a signed type to align with Kubernetes API conventions.
type Pid_t int64

type Executor interface {
	// Starts the process described by given command instance.
	// When the passed context is cancelled, the process is automatically terminated.
	// Returns the process PID and a function that enables process exit notifications delivered to the exit handler.
	StartProcess(ctx context.Context, cmd *exec.Cmd, exitHandler ProcessExitHandler) (pid Pid_t, startWaitForProcessExit func(), err error)

	// Stops the process with a given PID.
	StopProcess(pid Pid_t) error
}

type ProcessExitHandler interface {
	// Indicates that process with a given PID has finished execution
	// If err is nil, the process exit code was properly captured and the exitCode value is valid.
	// Note that if the process was terminated by a signal, the exit code will be UnknownExitCode (-1).
	// if err is not nil, there was a problem tracking the process and the exitCode value is not valid
	OnProcessExited(pid Pid_t, exitCode int32, err error)
}

// Make it easy to supply a function as a process exit handler.
type ProcessExitHandlerFunc func(Pid_t, int32, error)

func (f ProcessExitHandlerFunc) OnProcessExited(pid Pid_t, exitCode int32, err error) {
	f(pid, exitCode, err)
}
