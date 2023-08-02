package process

import (
	"context"
	"os/exec"

	ps "github.com/mitchellh/go-ps"

	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

// Runs the command as a child process to completion.
// Returns exit code, or error if the process could not be started/tracked for some reason.
func Run(ctx context.Context, executor Executor, cmd *exec.Cmd) (int32, error) {
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

// Returns the list of ID for a given process and its children
// The list is ordered starting with the root of the hierarchy, then the children, then the grandchildren etc.
func GetProcessTree(pid int32) ([]int32, error) {
	procs, err := ps.Processes()
	if err != nil {
		return nil, err
	}

	tree := []int32{}
	next := []int{int(pid)}

	for len(next) > 0 {
		current := next[0]
		next = next[1:]
		tree = append(tree, int32(current))
		children := slices.Select(procs, func(p ps.Process) bool {
			return p.PPid() == current
		})
		next = append(next, slices.Map[ps.Process, int](children, func(p ps.Process) int {
			return p.Pid()
		})...)
	}

	return tree, nil
}
