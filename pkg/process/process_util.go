// Copyright (c) Microsoft Corporation. All rights reserved.

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

	ps "github.com/shirou/gopsutil/v4/process"

	"github.com/microsoft/dcp/pkg/osutil"
	"github.com/microsoft/dcp/pkg/slices"
)

type ProcessTreeItem struct {
	Pid          Pid_t
	IdentityTime time.Time // Used to distinguish between different instances of processes with the same PID, may not be a valid wall-clock time.
}

var (
	This func() (ProcessTreeItem, error)

	// Essentially the same as ps.ErrorProcessNotRunning, but we do not want to
	// expose the ps package outside of this package.
	ErrorProcessNotFound = errors.New("process does not exist")
)

func getIDs(items []ProcessTreeItem) []Pid_t {
	return slices.Map[Pid_t](items, func(item ProcessTreeItem) Pid_t {
		return item.Pid
	})
}

// Returns the list of ID for a given process and its children
// The list is ordered starting with the root of the hierarchy, then the children, then the grandchildren etc.
func GetProcessTree(rootP ProcessTreeItem) ([]ProcessTreeItem, error) {
	root, err := findPsProcess(rootP.Pid, rootP.IdentityTime)
	if err != nil {
		return nil, err
	}

	tree := []ProcessTreeItem{}
	next := []*ps.Process{root}

	for len(next) > 0 {
		current := next[0]
		next = next[1:]
		nextPid := Uint32_ToPidT(uint32(current.Pid))
		tree = append(tree, ProcessTreeItem{nextPid, processIdentityTime(current)})

		children, childrenErr := current.Children()
		if childrenErr != nil {
			// If we fail to get the children, assume there are no children.
			children = []*ps.Process{}
		}

		next = append(next, children...)
	}

	return tree, nil
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

	_, _, startWaitForProcessExit, err := executor.StartProcess(ctx, cmd, peh, CreationFlagsNone)
	if err != nil {
		return UnknownExitCode, err
	}

	startWaitForProcessExit()

	// Only exit when the process exit--do not exit merely because the context is cancelled.
	exitInfo := <-pic
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
	case runResult := <-resultCh:
		return runResult.result, runResult.err
	}
}

// We serialize timestamps with millisecond precision, so a maximum couple of milliseconds of difference works well.
const ProcessIdentityTimeMaximumDifference = 2 * time.Millisecond

// Returns the creation time as a time.Time for a process.
// This time is intended for display purposes and may differ from the raw start time used to verify
// process identity and the value returned can change due to clock adjustments etc.
func StartTimeForProcess(pid Pid_t) time.Time {
	osPid, osPidErr := PidT_ToUint32(pid)
	if osPidErr != nil {
		return time.Time{}
	}

	proc, procErr := ps.NewProcess(int32(osPid))
	if procErr != nil {
		return time.Time{}
	}

	createTimestamp, err := proc.CreateTime()
	if err != nil {
		return time.Time{}
	}

	return time.UnixMilli(createTimestamp)
}

// Gets the raw start time for the process, used to verify process identity.
// This time may not match the wall clock time returned by StartTimeForProcess() on all OS platforms and
// should not be used for display purposes, but is stable across system clock changes.
func ProcessIdentityTime(pid Pid_t) time.Time {
	osPid, osPidErr := PidT_ToUint32(pid)
	if osPidErr != nil {
		return time.Time{}
	}

	proc, procErr := ps.NewProcess(int32(osPid))
	if procErr != nil {
		return time.Time{}
	}

	return processIdentityTime(proc)
}

func findPsProcess(pid Pid_t, expectedIdentityTime time.Time) (*ps.Process, error) {
	osPid, err := PidT_ToUint32(pid)
	if err != nil {
		return nil, err
	}

	// Call this first even if processStartTime is not used, to ensure the process exists.
	proc, procErr := ps.NewProcess(int32(osPid))
	if procErr != nil {
		if !errors.Is(procErr, ps.ErrorProcessNotRunning) {
			return nil, procErr
		} else {
			return nil, fmt.Errorf("process with pid %d does not exist: %w", pid, ErrorProcessNotFound)
		}
	}

	if !HasExpectedIdentityTime(proc, expectedIdentityTime) {
		actualIdentityTime := processIdentityTime(proc)

		return nil, fmt.Errorf(
			"process start time mismatch, pid might have been reused: pid %d, expected start time %s, actual start time %s",
			pid,
			expectedIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
			actualIdentityTime.Format(osutil.RFC3339MiliTimestampFormat),
		)
	}

	return proc, nil
}

// Returns the process with the given PID. If the expectedStartTime is not zero,
// the process start time is checked to match the expected start time.
func FindProcess(pid Pid_t, expectedStartTime time.Time) (*os.Process, error) {
	proc, err := findPsProcess(pid, expectedStartTime)
	if err != nil {
		return nil, err
	}

	process, err := os.FindProcess(int(proc.Pid))
	if err != nil {
		return nil, err
	}

	return process, nil
}

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

func HasExpectedIdentityTime(proc *ps.Process, expectedIdentityTime time.Time) bool {
	if expectedIdentityTime.IsZero() {
		return true
	} else {
		identityTime := processIdentityTime(proc)
		return osutil.Within(expectedIdentityTime, identityTime, ProcessIdentityTimeMaximumDifference)
	}
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

type Waitable interface {
	Wait() error
	Info() string
	Flags() ProcessCreationFlag
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

var _ Waitable = waitableCmd{}
var _ Waitable = waitableLite{}

func makeWaitable(pid Pid_t, proc *os.Process) Waitable {
	return waitableLite{
		wait: func() error {
			_, waitErr := proc.Wait()
			return waitErr
		},
		info: func() string {
			return "(" + strconv.FormatInt(int64(pid), 10) + ")"
		},
		flags: func() ProcessCreationFlag {
			return CreationFlagsNone
		},
	}
}

func init() {
	ps.EnableBootTimeCache(true)

	This = sync.OnceValues(func() (ProcessTreeItem, error) {
		retval := ProcessTreeItem{
			Pid:          UnknownPID,
			IdentityTime: time.Time{},
		}

		osPid := os.Getpid()
		pid := Uint32_ToPidT(uint32(osPid))

		pp, findProcessErr := ps.NewProcess(int32(osPid))
		if findProcessErr != nil {
			return retval, findProcessErr
		}

		identityTime := processIdentityTime(pp)

		retval.Pid = pid
		retval.IdentityTime = identityTime

		return retval, nil
	})
}
