package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/tklauser/ps"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

// Returns the list of ID for a given process and its children
// The list is ordered starting with the root of the hierarchy, then the children, then the grandchildren etc.
func GetProcessTree(pid Pid_t) ([]Pid_t, error) {
	procs, err := ps.Processes()
	if err != nil {
		return nil, err
	}

	tree := []Pid_t{}
	next := []Pid_t{pid}

	for len(next) > 0 {
		current := next[0]
		next = next[1:]
		tree = append(tree, current)

		children := slices.Select(procs, func(p ps.Process) bool {
			ppid, ppidErr := IntToPidT(p.PPID())
			if ppidErr != nil {
				panic(ppidErr)
			}
			return ppid == current
		})

		next = append(next, slices.Map[ps.Process, Pid_t](children, func(p ps.Process) Pid_t {
			processPID, pidConversionErr := IntToPidT(p.PID())
			if pidConversionErr != nil {
				panic(pidConversionErr)
			}
			return processPID
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

	if !expectedStartTime.IsZero() {
		actualStartTime := psProcess.CreationTime()

		if !osutil.Within(expectedStartTime, actualStartTime, ProcessStartTimestampMaximumDifference) {
			return nil, fmt.Errorf(
				"process start time mismatch, pid might have been reused: pid %d, expected start time %s, actual start time %s",
				pid,
				expectedStartTime.Format(osutil.RFC3339MiliTimestampFormat),
				actualStartTime.Format(osutil.RFC3339MiliTimestampFormat),
			)
		}
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
