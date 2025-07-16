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

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type ProcessTreeItem struct {
	Pid          Pid_t
	CreationTime time.Time
}

var (
	This func() (ProcessTreeItem, error)
)

func getIDs(items []ProcessTreeItem) []Pid_t {
	return slices.Map[ProcessTreeItem, Pid_t](items, func(item ProcessTreeItem) Pid_t {
		return item.Pid
	})
}

// Returns the list of ID for a given process and its children
// The list is ordered starting with the root of the hierarchy, then the children, then the grandchildren etc.
func GetProcessTree(rootP ProcessTreeItem) ([]ProcessTreeItem, error) {
	procs, err := ps.Processes()
	if err != nil {
		return nil, err
	}

	tree := []ProcessTreeItem{}
	next := []ProcessTreeItem{rootP}

	for len(next) > 0 {
		current := next[0]
		next = next[1:]
		tree = append(tree, current)

		children := slices.Select(procs, func(p *ps.Process) bool {
			osPpid, osPpidErr := p.Ppid()
			if osPpidErr != nil {
				return false // If we cannot get the parent PID, this process isn't a child
			}
			ppid, ppidErr := Uint32_ToPidT(uint32(osPpid))
			if ppidErr != nil {
				panic(ppidErr)
			}

			createTime := startTimeForProcess(p)

			return ppid == current.Pid && !createTime.Before(current.CreationTime)
		})

		next = append(next, slices.Map[*ps.Process, ProcessTreeItem](children, func(p *ps.Process) ProcessTreeItem {
			processPID, pidConversionErr := Uint32_ToPidT(uint32(p.Pid))
			if pidConversionErr != nil {
				panic(pidConversionErr)
			}

			creationTime := startTimeForProcess(p)

			return ProcessTreeItem{
				Pid:          processPID,
				CreationTime: creationTime,
			}
		})...)
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
const ProcessStartTimestampMaximumDifference = 2 * time.Millisecond

func startTimeForProcess(proc *ps.Process) time.Time {
	createTimestamp, err := proc.CreateTime()
	if err != nil {
		return time.Time{}
	}

	return time.UnixMilli(createTimestamp)
}

// Returns the creation time as a time.Time for a process.
// If the creation time cannot be retrieved, an error is returned.
func StartTimeForProcess(pid Pid_t) time.Time {
	osPid, osPidErr := PidT_ToUint32(pid)
	if osPidErr != nil {
		return time.Time{}
	}

	proc, procErr := ps.NewProcess(int32(osPid))
	if procErr != nil {
		return time.Time{}
	}

	return startTimeForProcess(proc)
}

// Returns the process with the given PID. If the expectedStartTime is not zero,
// the process start time is checked to match the expected start time.
func FindProcess(pid Pid_t, expectedStartTime time.Time) (*os.Process, error) {
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
			return nil, fmt.Errorf("process with pid %d does not exist", pid)
		}
	}

	if !HasExpectedStartTime(proc, expectedStartTime) {
		actualStartTime := startTimeForProcess(proc)

		return nil, fmt.Errorf(
			"process start time mismatch, pid might have been reused: pid %d, expected start time %s, actual start time %s",
			pid,
			expectedStartTime.Format(osutil.RFC3339MiliTimestampFormat),
			actualStartTime.Format(osutil.RFC3339MiliTimestampFormat),
		)
	}

	process, err := os.FindProcess(int(osPid))
	if err != nil {
		return nil, err
	}

	return process, nil
}

func Int64_ToPidT(val int64) (Pid_t, error) {
	return convertPid[int64, Pid_t](val)
}

func Uint32_ToPidT(val uint32) (Pid_t, error) {
	return convertPid[uint32, Pid_t](val)
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

func HasExpectedStartTime(proc *ps.Process, expectedStartTime time.Time) bool {
	if expectedStartTime.IsZero() {
		return true
	} else {
		creationTime := startTimeForProcess(proc)
		return osutil.Within(expectedStartTime, creationTime, ProcessStartTimestampMaximumDifference)
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
			CreationTime: time.Time{},
		}

		osPid := os.Getpid()
		pid, conversionErr := Uint32_ToPidT(uint32(osPid))
		if conversionErr != nil {
			return retval, conversionErr
		}

		pp, findProcessErr := ps.NewProcess(int32(osPid))
		if findProcessErr != nil {
			return retval, findProcessErr
		}

		startTime := startTimeForProcess(pp)

		retval.Pid = pid
		retval.CreationTime = startTime

		return retval, nil
	})
}
