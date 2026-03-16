/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

const (
	// A valid exit code of a process is a non-negative number. We use UnknownExitCode to indicate that we have not obtained the exit code yet.
	UnknownExitCode int32 = -1

	// Unknown PID code is used when replica is not started (or fails to start)
	UnknownPID Pid_t = -1
)

var (
	ErrTimedOutWaitingForProcessToStop = errors.New("timed out waiting for process to stop")
)

type ProcessCreationFlag uint32

const (
	CreationFlagsNone ProcessCreationFlag = 0

	// Ensures that the process is killed when process executor is disposed.
	// This has currently no effect on Mac/Linux, but on Windows the process started with this flag
	// will be added to the "killer job" object that terminates all processes assigned to it.
	CreationFlagEnsureKillOnDispose = 0x1
)

// Pid_t is a type used to represent process IDs.
// The domain is all unsigned 32-bit integers, but we use a signed type to align with Kubernetes API conventions.
type Pid_t int64

type Executor interface {
	// Starts the process described by given command instance.
	// When the passed context is cancelled, the process is automatically terminated.
	// Returns a ProcessHandle identifying the started process and a function that enables process exit notifications
	// delivered to the exit handler.
	StartProcess(
		ctx context.Context,
		cmd *exec.Cmd,
		exitHandler ProcessExitHandler,
		creationFlags ProcessCreationFlag,
	) (handle ProcessHandle, startWaitForProcessExit func(), err error)

	// Stops the process identified by the given ProcessHandle.
	// The handle's IdentityTime, if provided (time.IsZero() returns false), is used to further validate the process to be stopped
	// (to protect against stopping a wrong process, if the PID was reused).
	StopProcess(handle ProcessHandle) error

	// Starts a process that does not need to be tracked (the caller is not interested in its exit code),
	// minimizing resource usage. An error is returned if the process could not be started.
	StartAndForget(cmd *exec.Cmd, creationFlags ProcessCreationFlag) (handle ProcessHandle, err error)

	// Disposes the executor. Processes started with CreationFlagEnsureKillOnDispose will be terminated.
	// Other processes will be waited on (so that they do not become zombies), but not terminated.
	Dispose()
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

type ErrProcessNotFound struct {
	Pid   Pid_t
	Inner error
}

func (e ErrProcessNotFound) Error() string {
	if e.Inner == nil {
		return fmt.Sprintf("process %d not found", e.Pid)
	} else {
		return fmt.Sprintf("process %d not found: %v", e.Pid, e.Inner.Error())
	}
}

var _ error = ErrProcessNotFound{}
