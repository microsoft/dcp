package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/tklauser/ps"

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

		children := slices.Select(procs, func(p ps.Process) bool {
			ppid, ppidErr := IntToPidT(p.PPID())
			if ppidErr != nil {
				panic(ppidErr)
			}
			return ppid == current.Pid
		})

		next = append(next, slices.Map[ps.Process, ProcessTreeItem](children, func(p ps.Process) ProcessTreeItem {
			processPID, pidConversionErr := IntToPidT(p.PID())
			if pidConversionErr != nil {
				panic(pidConversionErr)
			}
			return ProcessTreeItem{
				Pid:          processPID,
				CreationTime: p.CreationTime(),
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

	_, _, startWaitForProcessExit, err := executor.StartProcess(ctx, cmd, peh)
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

// Returns the process with the given PID. If the expectedStartTime is not zero,
// the process start time is checked to match the expected start time.
func FindProcess(pid Pid_t, expectedStartTime time.Time) (*os.Process, error) {
	osPid, err := PidT_ToInt(pid)
	if err != nil {
		return nil, err
	}

	// Call this first even if processStartTime is not used, to ensure the process exists.
	psProcess, findErr := ps.FindProcess(osPid)
	if findErr != nil {
		return nil, findErr
	}

	if !HasExpectedStartTime(psProcess, expectedStartTime) {
		actualStartTime := psProcess.CreationTime()
		return nil, fmt.Errorf(
			"process start time mismatch, pid might have been reused: pid %d, expected start time %s, actual start time %s",
			pid,
			expectedStartTime.Format(osutil.RFC3339MiliTimestampFormat),
			actualStartTime.Format(osutil.RFC3339MiliTimestampFormat),
		)
	}

	process, err := os.FindProcess(osPid)
	if err != nil {
		return nil, err
	}

	return process, nil
}

func Int64ToPidT(val int64) (Pid_t, error) {
	return convertPid[int64, Pid_t](val)
}

func IntToPidT(val int) (Pid_t, error) {
	return convertPid[int, Pid_t](val)
}

func PidT_ToInt(val Pid_t) (int, error) {
	return convertPid[Pid_t, int](val)
}

func PidT_ToUint32(val Pid_t) (uint32, error) {
	return convertPid[Pid_t, uint32](val)
}

func StringToPidT(val string) (Pid_t, error) {
	i64val, i64ParseErr := strconv.ParseInt(val, 10, 64)
	if i64ParseErr != nil {
		return UnknownPID, i64ParseErr
	}

	return convertPid[int64, Pid_t](i64val)
}

func HasExpectedStartTime(psProcess ps.Process, expectedStartTime time.Time) bool {
	if expectedStartTime.IsZero() {
		return true
	} else {
		return osutil.Within(expectedStartTime, psProcess.CreationTime(), ProcessStartTimestampMaximumDifference)
	}
}

type Waitable interface {
	Wait() error
	Info() string
}

type waitableCmd struct {
	*exec.Cmd
}

func (cmd waitableCmd) Info() string {
	return fmt.Sprintf("Command %s", cmd.String())
}

type waitableLite struct {
	wait func() error
	info func() string
}

func (wl waitableLite) Info() string {
	return wl.info()
}

func (wl waitableLite) Wait() error {
	return wl.wait()
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
	}
}

func init() {
	This = sync.OnceValues(func() (ProcessTreeItem, error) {
		retval := ProcessTreeItem{
			Pid:          UnknownPID,
			CreationTime: time.Time{},
		}

		osPid := os.Getpid()
		pid, conversionErr := IntToPidT(osPid)
		if conversionErr != nil {
			return retval, conversionErr
		}

		pp, findProcessErr := ps.FindProcess(osPid)
		if findProcessErr != nil {
			return retval, findProcessErr
		}

		retval.Pid = pid
		retval.CreationTime = pp.CreationTime()

		return retval, nil
	})
}
