/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package process

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/microsoft/dcp/pkg/slices"
)

func FormatIdentityTime(identityTime time.Time) string {
	if identityTime.IsZero() {
		return ""
	}
	return formatIdentityTime(identityTime)
}

var (
	This func() (ProcessHandle, error)

	ErrorProcessNotFound          = errors.New("process does not exist")
	ErrInvalidProcessHandle       = errors.New("invalid process handle")
	ErrProcessIdentityUnavailable = errors.New("process identity is unavailable")
	ErrIncompleteProcessTree      = errors.New("process tree is incomplete")
	ErrProcessStartUncertain      = errors.New("process startup cleanup could not be confirmed")

	// Returned when a process with the requested PID exists, but its identity time
	// does not match the expected identity time. This typically means the original
	// process has exited and the PID has been reused by a different process.
	ErrProcessIdentityMismatch = errors.New("process start time mismatch, pid might have been reused")
)

// IsProcessGoneErr reports whether an error means the expected process exited, no longer exists, or its PID was reused.
func IsProcessGoneErr(err error) bool {
	if err == nil {
		return false
	}
	if joined, isJoined := err.(interface{ Unwrap() []error }); isJoined {
		innerErrors := joined.Unwrap()
		if len(innerErrors) == 0 {
			return false
		}
		for _, innerErr := range innerErrors {
			if !IsProcessGoneErr(innerErr) {
				return false
			}
		}
		return true
	}
	if _, notFound := err.(*ErrProcessNotFound); notFound {
		return true
	}
	if wrapped, isWrapped := err.(interface{ Unwrap() error }); isWrapped {
		return IsProcessGoneErr(wrapped.Unwrap())
	}
	return errors.Is(err, os.ErrProcessDone) ||
		errors.Is(err, ErrorProcessNotFound) ||
		errors.Is(err, ErrProcessIdentityMismatch)
}

func getIDs(items []ProcessHandle) []Pid_t {
	return slices.Map[Pid_t](items, func(item ProcessHandle) Pid_t {
		return item.Pid
	})
}

// Runs the command as a child process to completion.
// Returns exit code, or error if the process could not be started/tracked for some reason.
//
// The context parameter is used to request cancellation of the process, but the call to RunToCompletion() will not return
// until the process exits and all its output is captured.
// Do not assume the call will end quickly if the context is cancelled.
func RunToCompletion(ctx context.Context, executor Executor, cmd *exec.Cmd) (int32, error) {
	pic := make(chan ProcessExitInfo, 1)
	peh := NewChannelProcessExitHandler(pic)

	_, startWaitForProcessExit, startProcessErr := executor.StartProcess(ctx, cmd, peh, CreationFlagsNone, nil)
	if startProcessErr != nil {
		return UnknownExitCode, startProcessErr
	}

	startWaitForProcessExit()

	// Only exit when the process exit--do not exit merely because the context is cancelled.
	exitInfo, received := <-pic
	if !received {
		return UnknownExitCode, fmt.Errorf("process exit notification channel closed without a result")
	}
	return exitInfo.ExitCode, exitInfo.Err
}

type resultOrError[T any] struct {
	result T
	err    error
}

// Runs the command as a child process to completion, unless the passed context is cancelled,
// or its deadline is exceeded.
func RunWithTimeout(ctx context.Context, executor Executor, cmd *exec.Cmd) (int32, error) {
	resultCh := make(chan resultOrError[int32], 1)
	go func() {
		exitCode, err := RunToCompletion(ctx, executor, cmd)
		resultCh <- resultOrError[int32]{exitCode, err}
	}()

	select {
	case <-ctx.Done():
		return UnknownExitCode, ctx.Err()
	case runResult, received := <-resultCh:
		if !received {
			return UnknownExitCode, fmt.Errorf("process result channel closed without a result")
		}
		return runResult.result, runResult.err
	}
}

// We serialize timestamps with millisecond precision, so a maximum couple of milliseconds of difference works well.
const ProcessIdentityTimeMaximumDifference = 2 * time.Millisecond

func Int64_ToPidT(val int64) (Pid_t, error) {
	return convertPid[int64, Pid_t](val)
}

func Uint32_ToPidT(val uint32) Pid_t {
	// uint32 ia always valid as a PID value (see convertPid()), and can always be converted to Pid_t, which is int64-based.
	return Pid_t(val)
}

func PidT_ToInt(val Pid_t) (int, error) {
	return convertPid[Pid_t, int](val)
}

func PidT_ToUint32(val Pid_t) (uint32, error) {
	return convertPid[Pid_t, uint32](val)
}

func convertPid[From ~int64 | ~uint64 | ~uint32, To ~int64 | ~int | ~uint32](val From) (To, error) {
	outOfRange := val < 0 || val > math.MaxUint32
	if outOfRange {
		return 0, fmt.Errorf("value %d is out of range of valid process ID values", val)
	}
	return To(val), nil
}

func StringToPidT(val string) (Pid_t, error) {
	u64val, u64ParseErr := strconv.ParseUint(val, 10, 32)
	if u64ParseErr != nil {
		return UnknownPID, u64ParseErr
	}

	return convertPid[uint64, Pid_t](u64val)
}

// Checks if the error is associated with early exit of a process, which is often expected.
func IsEarlyProcessExitError(err error) bool {
	if err == nil {
		return false
	}

	var ee *exec.ExitError
	if errors.Is(err, os.ErrProcessDone) || errors.As(err, &ee) {
		// These are all expected errors, the process exited successfully.
		return true
	}

	// Receiving ECHILD when calling wait() on the child process is expected,
	// (the parent process might have terminated them).
	var sysErr *os.SyscallError
	isEChildErr := errors.As(err, &sysErr) && strings.Index(sysErr.Syscall, "wait") == 0 && errors.Is(sysErr.Err, syscall.ECHILD)
	return isEChildErr
}

type waitableCmd struct {
	*exec.Cmd
	flags ProcessCreationFlag
}

func (cmd waitableCmd) Info() string {
	return cmd.String()
}

func (cmd waitableCmd) Flags() ProcessCreationFlag {
	return cmd.flags
}

func (cmd waitableCmd) Abort(ctx context.Context) error {
	if cmd.WaitDelay == 0 || cmd.WaitDelay > waitForProcessExitTimeout {
		cmd.WaitDelay = waitForProcessExitTimeout
	}
	return rollbackProcessStart(ctx, cmd.Process.Kill, cmd.Wait)
}

type waitableLite struct {
	wait  func() error
	info  func() string
	flags func() ProcessCreationFlag
}

func (wl waitableLite) Info() string {
	return wl.info()
}

func (wl waitableLite) Wait() error {
	return wl.wait()
}

func (wl waitableLite) Flags() ProcessCreationFlag {
	return wl.flags()
}

func (wl waitableLite) Abort(_ context.Context) error {
	return fmt.Errorf("cannot roll back a discovered process: %w", errors.ErrUnsupported)
}

var _ Waitable = waitableCmd{}
var _ Waitable = waitableLite{}

func makeProcessWaitable(ctx context.Context, handle ProcessHandle) Waitable {
	return &waitableLite{
		wait: func() error {
			proc, findErr := FindProcess(handle)
			if IsProcessGoneErr(findErr) {
				return nil
			}
			if findErr != nil {
				return findErr
			}
			return waitForProcess(ctx, handle, proc, defaultWaitPollInterval)
		},
		info: func() string {
			return "(" + strconv.FormatInt(int64(handle.Pid), 10) + ")"
		},
		flags: func() ProcessCreationFlag {
			return CreationFlagsNone
		},
	}
}

func signalProcess(ctx context.Context, handle ProcessHandle, proc *os.Process, signal os.Signal) error {
	return actOnProcess(ctx, handle, func() (ProcessHandle, error) {
		info, infoErr := readProcessInfoFromProcess(proc, false)
		return info.handle, infoErr
	}, func() error {
		return proc.Signal(signal)
	})
}

func actOnProcess(ctx context.Context, handle ProcessHandle, inspect func() (ProcessHandle, error), action func() error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if handleErr := handle.Validate(); handleErr != nil {
		return handleErr
	}
	actual, inspectErr := inspect()
	if inspectErr != nil {
		return inspectErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if identityErr := validateIdentity(handle, actual); identityErr != nil {
		return identityErr
	}
	// Keep identity validation adjacent to dispatch. PID-based platforms still have a non-atomic race.
	return action()
}

func init() {
	This = sync.OnceValues(func() (ProcessHandle, error) {
		return FindProcessHandle(Uint32_ToPidT(uint32(os.Getpid())))
	})
}
