package process

import (
	"context"
	"os/exec"

	ps "github.com/mitchellh/go-ps"

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
			ppid, ppidErr := IntToPidT(p.PPid())
			if ppidErr != nil {
				panic(ppidErr)
			}
			return ppid == current
		})

		next = append(next, slices.Map[ps.Process, Pid_t](children, func(p ps.Process) Pid_t {
			processPID, pidConversionErr := IntToPidT(p.Pid())
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

	_, startWaitForProcessExit, err := executor.StartProcess(ctx, cmd, peh)
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
